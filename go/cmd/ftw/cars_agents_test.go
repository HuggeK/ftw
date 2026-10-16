package main

import (
	"bytes"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// carsOnEasee is a box with one Easee, the car made from it and a Volvo
// added since, wired as main.go wires a car change to the planner.
func carsOnEasee(t *testing.T, replan func(string)) (*loadpoint.Manager, *cars.Runtime, protocol.Car) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000}})
	rt := cars.New(cars.Deps{Store: &memRows{rows: map[string]string{}}, Loadpoints: mgr, Replan: replan,
		Vehicles: func() []telemetry.VehicleObservation { return nil }})
	if err := rt.Start(nil); err != nil {
		t.Fatal(err)
	}
	volvo, err := rt.Add(protocol.Car{ID: "0123456789abcdef0123456789abcdef", Name: "Familjens Volvo", VIN: "YV1XZK7V5N2000001",
		CapacityWh: 40000, Identifiers: []string{"rfid-7"}, Sources: []string{"volvo-cloud"}})
	if err != nil {
		t.Fatal(err)
	}
	return mgr, rt, volvo
}

// A tap reaches a streaming phone as a plan whose charger names the car, and
// the first such plan says it is being made: the phone never sees the new car
// beside the old plan as if that were current. The tap's replan is held in
// its read of the loadpoints until the test lets it go, so it is still being
// made when the phone hears of the car, as a real solve would be.
func TestATapReachesAStreamingPhoneWithTheCar(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hour := time.Now().UTC().Truncate(time.Hour)
	var prices []state.PricePoint
	for i := range 6 {
		prices = append(prices, state.PricePoint{
			Zone: "SE3", SlotTsMs: hour.Add(time.Duration(i) * time.Hour).UnixMilli(), SlotLenMin: 60,
			SpotOreKwh: float64(140 - i*20), TotalOreKwh: float64(190 - i*20), Source: "test", FetchedAtMs: hour.UnixMilli(),
		})
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	soc := 0.5
	tel.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	tel.DriverHealthMut("bat").RecordSuccess()
	svc := mpc.New(st, tel, "SE3", mpc.Params{Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5, CapacityWh: 10000,
		InitialSoC: .5, SoCMin: .1, SoCMax: .95, MaxChargeW: 3000, MaxDischargeW: 3000, ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.Horizon = 5 * time.Hour
	svc.BaseLoad = 500
	var held atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	svc.Loadpoints = func(int) []*mpc.LoadpointSpec {
		if held.Load() {
			<-release
		}
		return nil
	}

	mgr, rt, volvo := carsOnEasee(t, svc.RequestReplan)
	feed := appproto.NewPlanFeed(appproto.PlanSettle)
	svc.OnPlanChange(feed.Changed)
	owner := streamingSession(t, &appPlanChargers{planner: svc, mgr: mgr}, feed, apiauth.RoleOwner)

	mgr.ObserveSession("easee", true, 0, 0, true, "easee:EH1", "s1")
	rt.Tick()
	// The plug-in's own replan and the feed's settle pass first.
	for deadline := time.Now().Add(10 * time.Second); svc.IsReplanning() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * appproto.PlanSettle)

	held.Store(true)
	tapped := time.Now()
	if _, err := rt.SetSessionCar("easee", rt.ChargerCar("easee").Session, volvo.ID); err != nil {
		t.Fatal(err)
	}
	got := owner.waitPlan(t, func(p appproto.Plan) bool { return len(p.Chargers) == 1 && p.Chargers[0].CarID == volvo.ID })
	took := time.Since(tapped)
	if !got.Chargers[0].Pending {
		t.Fatalf("the first plan naming the Volvo is not pending: %+v", got.Chargers[0])
	}
	if took > appproto.PlanSettle+100*time.Millisecond {
		t.Fatalf("the tap reached the phone after %v", took)
	}
	letGo()
}

// An agent's redactor takes out what names a car, and its cars.get carries
// none of it.
func TestAgentsNeverSeeWhatNamesACar(t *testing.T) {
	_, rt, volvo := carsOnEasee(t, nil)
	p := &agentPorts{key: []byte("agent key"), cars: rt}
	line := "charged Familjens Volvo YV1XZK7V5N2000001 after rfid-7 via volvo-cloud"
	out := p.redactor().Text(line)
	for _, name := range []string{"Familjens Volvo", "YV1XZK7V5N2000001", "rfid-7", "volvo-cloud"} {
		if bytes.Contains([]byte(out), []byte(name)) {
			t.Fatalf("the redactor kept %q: %s", name, out)
		}
	}

	view, ok := agentViews{cars: rt}.Cars(p.key)
	if !ok || len(view.Cars) != 2 {
		t.Fatalf("cars.get %+v %v", view, ok)
	}
	raw, err := cloudwire.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Familjens Volvo", "YV1XZK7V5N2000001", "rfid-7", "volvo-cloud", volvo.ID} {
		if bytes.Contains(raw, []byte(name)) {
			t.Fatalf("cars.get names the car: %q", name)
		}
	}
	if _, ok := (agentViews{}).Cars(p.key); ok {
		t.Fatal("a box without cars has a car class")
	}
}
