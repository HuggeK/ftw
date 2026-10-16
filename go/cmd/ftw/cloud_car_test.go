package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// A charger's level and goal fields in FTW Cloud's copy are views of its
// session's car: a phone that joins carId to the car class finds the two
// agree, at each step of a session (docs/protocol-cloud.md, lp).
func TestTheChargerShowsItsCarAsTheCarClassDoes(t *testing.T) {
	now := time.Date(2026, 10, 16, 16, 30, 0, 0, time.UTC)
	mgr := loadpoint.NewManager()
	mgr.SetNowFn(func() time.Time { return now })
	mgr.SetLocation(time.UTC)
	mgr.Load([]loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000, MinChargeW: 4140, MaxChargeW: 11000}})
	rt := cars.New(cars.Deps{Store: &memRows{rows: map[string]string{}}, Loadpoints: mgr, Bus: events.NewBus(),
		Now: func() time.Time { return now }, Vehicles: func() []telemetry.VehicleObservation { return nil }})
	if err := rt.Start(nil); err != nil {
		t.Fatal(err)
	}
	mgr.SetLevelObserver(rt.LevelSet)
	volvo, err := rt.Add(protocol.Car{ID: "0123456789abcdef0123456789abcdef", Name: "Volvo", CapacityWh: 40000,
		Goal: &protocol.CarGoal{TargetPermille: 600, ReadyByMin: 390, Days: 0x7F}, SurplusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Add(protocol.Car{ID: "fedcba9876543210fedcba9876543210", Name: "Tesla", CapacityWh: 76000}); err != nil {
		t.Fatal(err)
	}
	step := func(d time.Duration, wh float64) {
		now = now.Add(d)
		mgr.ObserveSession("easee", true, 4140, wh, true, "easee:EH1", "s1")
		mgr.RollSchedules(now)
		rt.Tick()
	}
	step(0, 0)
	if _, err := rt.SetSessionCar("easee", rt.ChargerCar("easee").Session, volvo.ID); err != nil {
		t.Fatal(err)
	}
	step(time.Second, 0)

	site := &cloudSite{cars: rt}
	gk := []byte("grant key")
	agree := func(stage, source string) {
		t.Helper()
		st, _ := mgr.State("easee")
		lps := []cloudwire.Loadpoint{site.loadpoint(st, now)}
		site.joinCars(lps)
		lp := cloudlink.ProjectLP(gk, cloudwire.LP{Loadpoints: lps}).Loadpoints[0]
		var car *cloudwire.Car
		for _, c := range cloudlink.ProjectCars(gk, rt.Cars()).Cars {
			if c.ID == lp.CarID {
				car = &c
			}
		}
		if car == nil {
			t.Fatalf("%s: carId %q is no car in the car class", stage, lp.CarID)
		}
		if (lp.SocPermille == nil) != (car.SocPermille == nil) ||
			(lp.SocPermille != nil && *lp.SocPermille != *car.SocPermille) || lp.SocSource != source {
			t.Fatalf("%s: charger level %v %q, car %v %q estimate %v", stage, deref(lp.SocPermille), lp.SocSource,
				deref(car.SocPermille), car.SocSource, car.SocEstimate)
		}
		if lp.TargetPermille == nil || *lp.TargetPermille != car.Goal.TargetPermille || !lp.Recurring ||
			lp.ReadyByMs == nil || lp.SurplusOnly != car.SurplusOnly {
			t.Fatalf("%s: charger goal %v by %v recurring %v surplus %v, car %+v", stage, deref(lp.TargetPermille),
				deref(lp.ReadyByMs), lp.Recurring, lp.SurplusOnly, *car)
		}
	}

	// Nobody knows the Volvo's level: assumed.
	agree("no level", "assumed")
	// Someone sets it, and half a kWh flows: the car still shows the
	// number given, and so does the charger.
	if _, err := rt.SetLevel(volvo.ID, 0.42); err != nil {
		t.Fatal(err)
	}
	step(time.Second, 0)
	step(5*time.Minute, 500)
	agree("a level set, 0.5 kWh since", "inferred")
	// Past a kWh it is an estimate on both.
	step(10*time.Minute, 2500)
	agree("an estimate", "inferred")
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
