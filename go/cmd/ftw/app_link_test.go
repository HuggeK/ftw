package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func seedSite(t *testing.T) (*telemetry.Store, *control.State) {
	t.Helper()

	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "solar", "battery", "dead-solar"} {
		tel.EnsureDriverHealth(name)
	}
	soc := 0.62
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	// Negative while generating. FTW's convention is that positive watts flow
	// into the site, so PV is never positive.
	tel.Update("solar", telemetry.DerPV, -3400, nil, nil)
	tel.Update("battery", telemetry.DerBattery, 900, &soc, nil)
	tel.Update("dead-solar", telemetry.DerPV, -5000, nil, nil)
	for _, name := range []string{"meter", "solar", "battery"} {
		tel.RecordDriverSuccess(name)
	}
	tel.DriverHealthMut("dead-solar").SetOffline()

	return tel, &control.State{Mode: control.ModeSelfConsumption, SiteMeterDriver: "meter"}
}

// The signs are the invariant. A PV field that arrived positive would make
// every sentence the app writes about solar the opposite of the truth, and no
// test in the app can catch it because the app believes what it is sent.
func TestAppSnapshotKeepsTheSiteSignConvention(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()

	if snap.GridW != 1200 {
		t.Fatalf("grid = %v, want 1200 drawing from the grid", snap.GridW)
	}
	if snap.PVW != -3400 {
		t.Fatalf("pv = %v; PV is never positive", snap.PVW)
	}
	if snap.BatteryW != 900 {
		t.Fatalf("battery = %v, want 900 charging", snap.BatteryW)
	}
	// grid = load + battery + pv, rearranged.
	if snap.LoadW != 1200-900+3400 {
		t.Fatalf("load = %v, want %v", snap.LoadW, 1200-900+3400)
	}
	if !snap.BatterySoCKnown || snap.BatterySoC != 0.62 {
		t.Fatalf("soc = %v (known %v), want the fraction 0.62", snap.BatterySoC, snap.BatterySoCKnown)
	}
}

// The LAN dashboard splits house and car from every charger that is still
// reporting, including one that cannot take a command. The app snapshot has
// to do the same: hiding that draw in load_w is how the phone showed EV at
// 0 W and house at house+car.
func TestAppSnapshotSplitsEVFromHouseLoad(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 2000, nil, nil)
	tel.RecordDriverSuccess("easee")

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown {
		t.Fatal("a site with a charger did not send field 10")
	}
	if snap.EVW != 2000 {
		t.Fatalf("ev = %v, want 2000", snap.EVW)
	}
	// grid 1200, battery +900, PV -3400, EV 2000 → house 1700.
	if snap.LoadW != 1700 {
		t.Fatalf("load = %v, want 1700 (house, not house+car)", snap.LoadW)
	}
}

func TestAppSnapshotCountsAFaultedChargersDraw(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 2000, nil, nil)
	tel.RecordDriverSuccess("easee")
	tel.SetDriverDeviceFault("easee", true, "setpoint refused")

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown || snap.EVW != 2000 {
		t.Fatalf("ev = %v known=%v; a faulted charger is still drawing", snap.EVW, snap.EVWKnown)
	}
	if snap.LoadW != 1700 {
		t.Fatalf("load = %v; the car's draw landed in the house", snap.LoadW)
	}
}

func TestAppSnapshotIgnoresAnOfflineChargersDraw(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 11400, nil, nil)
	tel.RecordDriverSuccess("easee")
	tel.DriverHealthMut("easee").SetOffline()

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown {
		t.Fatal("an idle-looking charger that exists must still be named")
	}
	if snap.EVW != 0 {
		t.Fatalf("ev = %v; an offline charger's last-known draw leaked", snap.EVW)
	}
	// grid 1200 - bat 900 - pv -3400 = 3700, no EV subtracted.
	if snap.LoadW != 1200-900+3400 {
		t.Fatalf("load = %v, want house without a live car", snap.LoadW)
	}
}

// An offline driver contributes nothing. Adding its last reading would show a
// number that is not happening as though it were.
func TestAppSnapshotIgnoresAnOfflineDriversReading(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()

	if snap.PVW != -3400 {
		t.Fatalf("pv = %v; the offline array was counted", snap.PVW)
	}

	// It still appears as a source, marked offline. The app needs to say the
	// array went quiet, which it cannot do if the box stops mentioning it.
	var found bool
	for _, src := range snap.Sources {
		if src.ID == "dead-solar" {
			found = true
			if !src.Offline {
				t.Fatal("the offline array is not marked offline")
			}
		}
	}
	if !found {
		t.Fatal("the offline array vanished from the source list")
	}
}

// Stale site-meter data stops dispatch. Naming the source is what lets the app
// say why nothing is happening instead of looking broken.
func TestAnOfflineSiteMeterBlocksDispatch(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.DriverHealthMut("meter").SetOffline()

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()
	if len(snap.DispatchBlockedBy) != 1 || snap.DispatchBlockedBy[0] != "meter" {
		t.Fatalf("dispatchBlockedBy = %v, want the site meter", snap.DispatchBlockedBy)
	}
}

// The revision has to move when the mode does, or the app's expect.rev check
// passes everything and reads like a conflict check that is not one.
func TestAppSnapshotRevisionFollowsTheControlState(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	before := site.Snapshot().ControlRev
	if same := site.Snapshot().ControlRev; same != before {
		t.Fatalf("the revision moved on an unchanged site: %d → %d", before, same)
	}

	ctrl.Mode = control.ModePeakShaving
	if after := site.Snapshot().ControlRev; after <= before {
		t.Fatalf("the revision is %d after a mode change, was %d", after, before)
	}
}

// A mode change is three things. Arriving with two of them has already cost a
// night's battery, which is why every door goes through control.ApplyMode.
func TestAppSetModeGoesThroughApplyMode(t *testing.T) {
	_, ctrl := seedSite(t)
	ctrl.PI = &control.PIController{}
	ctrl.SetBatteryManualHold(control.BatteryManualHold{
		Driver: "battery", PowerW: -2000, ExpiresAt: time.Now().Add(time.Hour),
	})

	modes := &appModes{ctrl: ctrl, ctrlMu: &sync.Mutex{}}
	if err := modes.SetMode(t.Context(), control.ModePeakShaving); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	if ctrl.Mode != control.ModePeakShaving {
		t.Fatalf("mode = %q", ctrl.Mode)
	}
	if _, active := ctrl.GetBatteryManualHold(time.Now()); active {
		t.Fatal("the manual hold survived a mode change")
	}
}

// Home Assistant's mode command is the app's door. It used to be a third copy
// that returned when the mode could not be saved, before the export preference
// and the planner were told, so control ran one strategy while the planner
// planned another.
func TestHomeAssistantSetModeFinishesWhenTheModeCannotBeSaved(t *testing.T) {
	_, ctrl := seedSite(t)
	ctrl.PI = &control.PIController{}
	ctrl.SetBatteryManualHold(control.BatteryManualHold{
		Driver: "battery", PowerW: -2000, ExpiresAt: time.Now().Add(time.Hour),
	})
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	prefs := config.NewPlannerPrefs(config.ForecastTrustBalanced, config.BatteryExportNotAllowed, config.SafetyKDefault)

	callbacks := haCallbacks(ctrl, &sync.Mutex{}, st, nil, prefs)
	if err := callbacks.SetMode(string(control.ModePlannerArbitrage)); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	if ctrl.Mode != control.ModePlannerArbitrage {
		t.Fatalf("mode = %q", ctrl.Mode)
	}
	if _, active := ctrl.GetBatteryManualHold(time.Now()); active {
		t.Fatal("the manual hold survived a mode change")
	}
	if _, export, _ := prefs.Get(); export != config.BatteryExportAllowed {
		t.Fatalf("battery export = %q; the mode change stopped before the planner preference", export)
	}
	if err := callbacks.SetMode("priority"); err == nil {
		t.Fatal("Home Assistant set a removed mode")
	}
	if ctrl.Mode != control.ModePlannerArbitrage {
		t.Fatalf("a refused mode changed the state to %q", ctrl.Mode)
	}
}

func TestAppSetModeRefusesAModeTheBoxDoesNotHave(t *testing.T) {
	_, ctrl := seedSite(t)
	modes := &appModes{ctrl: ctrl, ctrlMu: &sync.Mutex{}}

	if err := modes.SetMode(t.Context(), control.Mode("turbo")); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("a refused mode still changed the state to %q", ctrl.Mode)
	}
}

// The port's charge-level path is the HTTP route's: SetCurrentSoC on the
// manager, refused while no car is plugged in, and read back from the
// manager's own state — within the half-permille the handler allows, because
// the manager re-anchors through the session's delivered energy.
func TestAppLoadpointsCorrectTheChargeLevelThroughTheManager(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{
		ID: "garage", DriverName: "easee-cloud",
		VehicleCapacityWh: 60000, PluginSoC: 0.4,
	}})
	lp := &appLoadpoints{mgr: mgr}

	if lp.SetSoC("garage", 0.62) {
		t.Fatal("an unplugged car's level was set")
	}

	mgr.Observe("garage", true, 7400, 1200, true) // 1.2 kWh into the session
	if !lp.SetSoC("garage", 0.62) {
		t.Fatal("a plugged-in car's level was refused")
	}
	got, ok := lp.ObservedSoC("garage")
	if !ok || math.Abs(got-0.62) > 0.0005 {
		t.Fatalf("read back %v (known %v), want 0.62", got, ok)
	}
	if _, ok := lp.ObservedSoC("street"); ok {
		t.Fatal("a loadpoint the box does not have read back a level")
	}
}

// The port's PV-only path is the HTTP target route's SetSurplusOnly: the
// previous value comes back so the caller knows the direction, and the
// read-back is the manager's own flag.
func TestAppLoadpointsFlipSurplusOnlyThroughTheManager(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee-cloud", SurplusOnly: true}})
	lp := &appLoadpoints{mgr: mgr}

	prev, ok := lp.SetSurplusOnly("garage", false)
	if !ok || !prev {
		t.Fatalf("SetSurplusOnly = (%v, %v), want the previous true", prev, ok)
	}
	if v, ok := lp.ObservedSurplusOnly("garage"); !ok || v {
		t.Fatalf("read back %v (known %v), want off", v, ok)
	}

	prev, ok = lp.SetSurplusOnly("garage", true)
	if !ok || prev {
		t.Fatalf("SetSurplusOnly = (%v, %v), want the previous false", prev, ok)
	}
	if v, ok := lp.ObservedSurplusOnly("garage"); !ok || !v {
		t.Fatalf("read back %v (known %v), want on", v, ok)
	}

	if _, ok := lp.SetSurplusOnly("street", true); ok {
		t.Fatal("a loadpoint the box does not have took a flag")
	}
}

// The home box's Sungrow, read on 14 October 2026: it gives each phase's
// current as a size and each phase's power with the site's sign. One phase
// imported while the other two exported, so the wire must say which is
// which, and the fuse travels with them as their scale.
func TestAppSnapshotSignsEachPhaseByItsPower(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("meter", telemetry.DerMeter, 160, nil, json.RawMessage(
		`{"w":160,"l1_w":-367,"l2_w":908,"l3_w":-381,"l1_a":2.13,"l2_a":4.76,"l3_a":1.61}`))
	ctrl.SiteFuseAmps, ctrl.SiteFusePhases = 20, 3
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()

	want := [3]float64{-2.13, 4.76, -1.61}
	for i, w := range want {
		if snap.GridPhaseA[i] == nil || *snap.GridPhaseA[i] != w {
			t.Fatalf("L%d = %v, want %v", i+1, snap.GridPhaseA[i], w)
		}
	}
	if snap.FuseA != 20 {
		t.Fatalf("fuse = %v, want 20", snap.FuseA)
	}
}

func TestGridPhaseAmps(t *testing.T) {
	reading := func(data string) *telemetry.DerReading {
		return &telemetry.DerReading{Data: json.RawMessage(data)}
	}
	show := func(p [3]*float64) []any {
		out := make([]any, 3)
		for i, v := range p {
			if v != nil {
				out[i] = *v
			}
		}
		return out
	}
	cases := []struct {
		name   string
		data   string
		phases int
		want   []any
	}{
		{
			// A meter that signs its currents and gives no power keeps them.
			name: "signed by the driver", phases: 3,
			data: `{"l1_a":-17,"l2_a":3,"l3_a":0}`,
			want: []any{-17.0, 3.0, 0.0},
		},
		{
			// A P1 meter: sizes only. Positive is all it can say.
			name: "sizes only", phases: 3,
			data: `{"l1_a":4.2,"l2_a":0.4,"l3_a":1.1}`,
			want: []any{4.2, 0.4, 1.1},
		},
		{
			// The power is the site's sign, set by the driver. Copying it
			// onto a current already signed the same way changes nothing.
			name: "power agrees with a signed current", phases: 3,
			data: `{"l1_a":-6,"l1_w":-1380,"l2_a":2,"l2_w":460,"l3_a":1,"l3_w":0}`,
			want: []any{-6.0, 2.0, 1.0},
		},
		{
			// A one-phase site owns no L2, whatever the payload holds.
			name: "one phase", phases: 1,
			data: `{"l1_a":9.5,"l1_w":2185,"l2_a":3,"l3_a":3}`,
			want: []any{9.5, nil, nil},
		},
		{
			name: "a phase the meter does not report", phases: 3,
			data: `{"l1_a":1,"l3_a":2}`,
			want: []any{1.0, nil, 2.0},
		},
		{
			// Harnesses that leave fuse.phases unset read three, as the
			// fuse guard does.
			name: "phases unset", phases: 0,
			data: `{"l1_a":1,"l2_a":2,"l3_a":3}`,
			want: []any{1.0, 2.0, 3.0},
		},
		{
			name: "no phases at all", phases: 3,
			data: `{"w":1200}`,
			want: []any{nil, nil, nil},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := show(gridPhaseAmps(reading(c.data), c.phases))
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("L%d = %v, want %v (all: %v)", i+1, got[i], c.want[i], got)
				}
			}
		})
	}
	if got := gridPhaseAmps(nil, 3); got != [3]*float64{} {
		t.Fatalf("no reading gave %v", show(got))
	}
}

// An offline meter's phases are as old as its power. The power already goes
// unsent; the phases go with it, so the app is never handed a phase it would
// have to age by itself.
func TestAnOfflineSiteMeterSendsNoPhases(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("meter", telemetry.DerMeter, 160, nil, json.RawMessage(`{"l1_a":2.13,"l2_a":4.76,"l3_a":1.61}`))
	tel.DriverHealthMut("meter").SetOffline()
	ctrl.SiteFuseAmps, ctrl.SiteFusePhases = 20, 3
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	if got := site.Snapshot().GridPhaseA; got != [3]*float64{} {
		t.Fatalf("an offline meter sent phases %v", got)
	}
}
