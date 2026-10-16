package control

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func pendingEVFixture(t *testing.T, house, battery, ev, rawBattery float64) (*telemetry.Store, *State, time.Time) {
	t.Helper()
	now, store := time.Now(), telemetry.NewStore()
	emit := func(driver string, kind telemetry.DerType, at time.Time, raw, ac float64) {
		d := map[string]any{"control_power_w": ac, "control_power_observed_at": at.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			amps := math.Abs(ac) / 690
			d["l1_a"], d["l2_a"], d["l3_a"] = amps, amps, amps
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, raw, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	emit("battery", telemetry.DerBattery, now.Add(-8*time.Second), rawBattery, battery)
	emit("car", telemetry.DerEV, now.Add(-8*time.Second), ev, ev)
	emit("meter", telemetry.DerMeter, now.Add(-7*time.Second), house+battery+ev, house+battery+ev)
	emit("battery", telemetry.DerBattery, now.Add(-6*time.Second), rawBattery, battery)
	emit("car", telemetry.DerEV, now.Add(-6*time.Second), ev, ev)
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	return store, state, now
}

func pendingEVOrder(store *telemetry.Store, watts float64, at time.Time, result string) {
	c := store.BeginCommand("battery", []byte(fmt.Sprintf(`{"action":"battery","power_w":%g}`, watts)), at)
	store.CompleteCommand(c, result)
}

func pendingEVBudget(store *telemetry.Store, state *State) float64 {
	cap, _ := EVFuseBudget(store, state, []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}},
		[]loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
	return cap
}

func TestEVPendingBatteryCannotSpendAnUnmeasuredOrder(t *testing.T) {
	for _, lower := range []bool{false, true} {
		for _, result := range []string{"accepted", "failed", "unknown"} {
			t.Run(fmt.Sprintf("lower=%v/%s", lower, result), func(t *testing.T) {
				store, state, now := pendingEVFixture(t, 5000, 0, 3000, 0)
				pendingEVOrder(store, 8000, now.Add(-4*time.Second), result)
				state.PrevTargets["battery"] = 8000
				if lower {
					pendingEVOrder(store, 0, now.Add(-3*time.Second), "accepted")
					state.PrevTargets["battery"] = 0
				}
				got := pendingEVBudget(store, state)
				const want = 16560 - 5000 - 8000
				if math.Abs(got-want) > .01 {
					t.Fatalf("EV cap %.1fW; need %.1fW before 8kW battery order can arrive", got, float64(want))
				}
				if grid := 5000 + 8000 + got; grid > 16560.01 {
					t.Fatalf("pending battery produces %.1fW physical import", grid)
				}
			})
		}
	}
}

func TestEVPendingBatteryReservesDischargeLossAndUsesAC(t *testing.T) {
	// Site includes -5kW AC discharge. Its DC reading is deliberately different.
	store, state, now := pendingEVFixture(t, 10000, -5000, 3000, -300)
	pendingEVOrder(store, 0, now.Add(-4*time.Second), "accepted")
	got := pendingEVBudget(store, state)
	if want := 16560.0 - 10000; math.Abs(got-want) > .01 {
		t.Fatalf("loss of AC discharge not reserved: cap %.1fW want %.1fW", got, want)
	}
}

func TestEVPendingBatteryCannotUseLaterPowerAsReplacementCredit(t *testing.T) {
	store, state, now := pendingEVFixture(t, 5000, 0, 3000, 0)
	pendingEVOrder(store, 8000, now.Add(-4*time.Second), "accepted")
	// Battery feedback is newer than the site and has no matching site sample.
	data, _ := json.Marshal(map[string]any{"control_power_observed_at": now.Add(-time.Second).Format(time.RFC3339Nano)})
	soc := .5
	store.Update("battery", telemetry.DerBattery, 8000, &soc, data)
	if got := pendingEVBudget(store, state); math.Abs(got-3560) > .01 {
		t.Fatalf("newer battery reading financed an EV rise: cap %.1fW", got)
	}
}

func TestEVPendingBatteryRecoveryIgnoresOldAllocatorCapButKeepsPeak(t *testing.T) {
	store, state, _ := pendingEVFixture(t, 2000, 0, 0, 0)
	state.FuseSaturated, state.FuseEVMaxW = true, 0
	if got := pendingEVBudget(store, state); math.Abs(got-14560) > .01 {
		t.Fatalf("old allocator cap blocked measured room: %.1fW", got)
	}
	state.PeakImportCeilingW = 6000
	if got := pendingEVBudget(store, state); math.Abs(got-4000) > .01 {
		t.Fatalf("current tariff peak lost: %.1fW", got)
	}
}

func TestEVPendingBatteryMissingPowerCannotInventRoom(t *testing.T) {
	store, state, now := pendingEVFixture(t, 5000, 0, 3000, 0)
	pendingEVOrder(store, 8000, now.Add(-4*time.Second), "accepted")
	data := []byte(`{"control_power_available":false}`)
	store.Update("battery", telemetry.DerBattery, 0, nil, data)
	if got := pendingEVBudget(store, state); got != 0 {
		t.Fatalf("missing battery power earned %.1fW EV room", got)
	}
}

func TestEVPendingBatteryReservesComputedOfferWaitingForActor(t *testing.T) {
	store, state, now := pendingEVFixture(t, 5000, 0, 3000, 0)
	pendingEVOrder(store, 0, now.Add(-5*time.Second), "accepted")
	offeredAt := now.Add(-4 * time.Second)
	state.PrevTargets["battery"], state.LastDispatch = 8000, &offeredAt
	if got := pendingEVBudget(store, state); math.Abs(got-3560) > .01 {
		t.Fatalf("computed battery offer waiting for actor funded %.1fW EV", got)
	}
}

func TestEVPendingBatteryFirstSampleWaitsForSiteCorrespondence(t *testing.T) {
	store, state, now := pendingEVFixture(t, 10000, -5000, 3000, -5000)
	// Restart loses the battery history. The next zero reading cannot tell
	// us how much discharge the older 8kW site sample still contains.
	store.Remove("battery")
	pendingEVOrder(store, 0, now.Add(-4*time.Second), "accepted")
	emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64) {
		data, _ := json.Marshal(map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)})
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	emit("battery", telemetry.DerBattery, now.Add(-3*time.Second), 0)
	if got := pendingEVBudget(store, state); got > 1560.01 {
		t.Fatalf("unknown earlier discharge financed %.1fW EV room", got)
	}
	// The next bracket proves the new balance and releases that startup bound.
	amps := 13000.0 / 690
	data, _ := json.Marshal(map[string]any{"control_power_observed_at": now.Add(-2 * time.Second).Format(time.RFC3339Nano),
		"l1_a": amps, "l2_a": amps, "l3_a": amps})
	store.Update("meter", telemetry.DerMeter, 13000, nil, data)
	emit("battery", telemetry.DerBattery, now.Add(-time.Second), 0)
	emit("car", telemetry.DerEV, now.Add(-time.Second), 3000)
	if got := pendingEVBudget(store, state); math.Abs(got-6560) > .01 {
		t.Fatalf("matching fresh readings did not restore room: %.1fW", got)
	}
}

func TestEVPendingBatteryOrderArrivalKeepsPhysicalImportBounded(t *testing.T) {
	rng := rand.New(rand.NewSource(8000))
	for i := 0; i < 200; i++ {
		house := 1000 + rng.Float64()*6000
		actual := -5000 + rng.Float64()*10000
		target := -5000 + rng.Float64()*15000
		if house+math.Max(actual, target) > 16560 {
			continue // The base load alone is already impossible to fix with EV.
		}
		store, state, now := pendingEVFixture(t, house, actual, 3000, actual)
		pendingEVOrder(store, target, now.Add(-4*time.Second), "accepted")
		ev := pendingEVBudget(store, state)
		if grid := house + math.Max(actual, target) + ev; grid > 16560.01 {
			t.Fatalf("case %d: late signed order gives %.1fW import; house %.1f actual %.1f target %.1f EV %.1f", i, grid, house, actual, target, ev)
		}
	}
}

func TestEVPendingBatterySteadyEarlierPollDoesNotBlockEVStart(t *testing.T) {
	now, store := time.Now(), telemetry.NewStore()
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	pendingEVOrder(store, 8000, now.Add(-19*time.Second), "accepted")
	emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64) {
		d := map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			amps := watts / 690
			d["l1_a"], d["l2_a"], d["l3_a"] = amps, amps, amps
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	for i := 0; i < 6; i++ {
		at := now.Add(-8*time.Second + time.Duration(i)*1250*time.Millisecond)
		// Every battery poll precedes its site poll. No latest site sample
		// ever has an after bracket, but the preceding site sample does.
		emit("battery", telemetry.DerBattery, at, 8000)
		emit("car", telemetry.DerEV, at, 0)
		emit("meter", telemetry.DerMeter, at.Add(250*time.Millisecond), 9000)
		if got := pendingEVBudget(store, state); i > 0 && math.Abs(got-7560) > .01 {
			t.Fatalf("tick %d: steady earlier battery poll blocked EV start: %.1fW", i, got)
		}
	}
	// A newer house-load increase still wins over the older common sample.
	emit("meter", telemetry.DerMeter, now.Add(-100*time.Millisecond), 14000)
	if got := pendingEVBudget(store, state); got > 2560.01 {
		t.Fatalf("older coherent site hid newer load: %.1fW", got)
	}
}

func TestEVPendingBatteryObservedStopCannotDisappearFromCredit(t *testing.T) {
	store := telemetry.NewStore()
	at := time.Now().Truncate(time.Second).Add(-2 * time.Second)
	const house = 5000.0
	emit := func(driver string, kind telemetry.DerType, sourceAt time.Time, w float64) {
		d := map[string]any{"control_power_observed_at": sourceAt.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			a := w / 690
			d["l1_a"], d["l2_a"], d["l3_a"] = a, a, a
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, w, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	pendingEVOrder(store, 8000, at.Add(-time.Second), "accepted")
	emit("battery", telemetry.DerBattery, at.Add(-100*time.Millisecond), 8000)
	emit("battery", telemetry.DerBattery, at.Add(100*time.Millisecond), 0)
	emit("car", telemetry.DerEV, at.Add(400*time.Millisecond), 0)
	emit("meter", telemetry.DerMeter, at.Add(500*time.Millisecond), house)
	emit("battery", telemetry.DerBattery, at.Add(900*time.Millisecond), 8000)
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	capW := pendingEVBudget(store, state)
	windows := store.ControlWindows(time.Now().Add(-20*time.Second), time.Now())
	t.Logf("battery points=%+v cap=%.0fW latest battery=8000W site snapshot=5000W actual import after offer=%.0fW", windows["battery:battery"].Points, capW, house+8000+capW)
	if total := house + 8000 + capW; total > 16560.01 {
		t.Fatalf("observed zero lost from battery credit: actual import %.0fW, cap %.0fW (safe <=3560W)", total, capW)
	}
}

func TestEVPendingBatteryNewMeasuredACPowerBeforeSiteMustBeReserved(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		for _, tc := range []struct {
			name                        string
			house, before, after, dcRaw float64
			wantCap                     float64
		}{
			{"new_charge", 5000, 0, 8000, 2000, 3560},
			{"lost_discharge", 10000, -5000, 0, -300, 6560},
		} {
			t.Run(fmt.Sprintf("%s/order=%v", tc.name, ordered), func(t *testing.T) {
				store, state, now := pendingEVFixture(t, tc.house, tc.before, 3000, tc.before)
				if ordered {
					pendingEVOrder(store, 0, now.Add(-10*time.Second), "accepted")
				}
				data, _ := json.Marshal(map[string]any{"control_power_w": tc.after,
					"control_power_observed_at": now.Add(-time.Second).Format(time.RFC3339Nano)})
				soc := .5
				store.Update("battery", telemetry.DerBattery, tc.dcRaw, &soc, data)
				cap := pendingEVBudget(store, state)
				if math.Abs(cap-tc.wantCap) > .01 {
					t.Fatalf("new measured AC power missing from site balance: cap%.1f want%.1f", cap, tc.wantCap)
				}
				if grid := tc.house + tc.after + cap; grid > 16560.01 {
					t.Fatalf("observed AC change gives %.1fW physical import", grid)
				}
			})
		}
	}
}

func TestEVPendingBatteryMeasuredRiseDoesNotCountTwiceAcrossPolls(t *testing.T) {
	now, store := time.Now(), telemetry.NewStore()
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64) {
		d := map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			d["l1_a"], d["l2_a"], d["l3_a"] = watts/690, watts/690, watts/690
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	start := now.Add(-8 * time.Second)
	emit("battery", telemetry.DerBattery, start, 0)
	emit("car", telemetry.DerEV, start, 10350)
	emit("meter", telemetry.DerMeter, start.Add(250*time.Millisecond), 15350)
	emit("battery", telemetry.DerBattery, start.Add(500*time.Millisecond), 0)
	emit("car", telemetry.DerEV, start.Add(500*time.Millisecond), 10350)
	pendingEVOrder(store, 1210, start.Add(750*time.Millisecond), "accepted")
	for tick := range 6 {
		at := start.Add(time.Duration(tick+1) * time.Second)
		emit("battery", telemetry.DerBattery, at, 1210)
		emit("car", telemetry.DerEV, at, 10350)
		emit("meter", telemetry.DerMeter, at.Add(250*time.Millisecond), 16560)
		emit("car", telemetry.DerEV, at.Add(500*time.Millisecond), 10350)
		if cap := pendingEVBudget(store, state); math.Abs(cap-10350) > .01 {
			t.Fatalf("tick%d: measured battery rise counted twice, EVcap%.1f want10350; physical5000+1210+10350=16560", tick, cap)
		}
	}
}
