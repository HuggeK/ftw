package main

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

type memRows struct {
	mu   sync.Mutex
	rows map[string]string
}

func (m *memRows) LoadConfig(k string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows[k]
	return v, ok
}

func (m *memRows) SaveConfig(k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[k] = v
	return nil
}

// Before cars the planner sized every plan from the charger's configured
// capacity (main.go read cfg.Loadpoints[].VehicleCapacityWh), whatever car
// was plugged in. Two cars on one Easee: the box asks which, a tap names the
// Volvo, and the plan's inputs are the Volvo's capacity, goal, level and the
// power its own charger takes.
func TestThePlanFollowsTheTappedCar(t *testing.T) {
	now := time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC)
	mgr := loadpoint.NewManager()
	mgr.SetNowFn(func() time.Time { return now })
	mgr.SetLocation(time.UTC)
	mgr.Load([]loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000, MinChargeW: 4140, MaxChargeW: 11000,
		AllowedStepsW: []float64{4140, 7360, 11000}}})
	var goal loadpoint.Schedule
	if err := json.Unmarshal([]byte(`{"soc":0.8,"recurring":true,"time_of_day_min":420}`), &goal); err != nil {
		t.Fatal(err)
	}
	if err := mgr.HydrateSchedules(func(string) (loadpoint.Schedule, bool) { return goal, true }); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	var asked []events.CarWhich
	bus.Subscribe(events.KindCarWhich, func(e events.Event) { asked = append(asked, e.(events.CarWhich)) })
	rt := cars.New(cars.Deps{Store: &memRows{rows: map[string]string{}}, Loadpoints: mgr, Bus: bus,
		Now: func() time.Time { return now }, Vehicles: func() []telemetry.VehicleObservation { return nil }})
	if err := rt.Start(nil); err != nil {
		t.Fatal(err)
	}
	volvo, err := rt.Add(protocol.Car{ID: "0123456789abcdef0123456789abcdef", Name: "Volvo", CapacityWh: 40000, MaxW: 7400,
		Goal: &protocol.CarGoal{TargetPermille: 600, ReadyByMin: 390, Days: 0x7F}})
	if err != nil {
		t.Fatal(err)
	}

	mgr.ObserveSession("easee", true, 0, 0, true, "easee:EH1", "s1")
	for i := 0; i < 3; i++ {
		now = now.Add(31 * time.Second)
		mgr.RollSchedules(now)
		rt.Tick()
	}
	if len(asked) != 1 {
		t.Fatalf("car.which raised %d times, want once", len(asked))
	}
	if _, err := rt.SetSessionCar("easee", rt.ChargerCar("easee").Session, volvo.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	mgr.RollSchedules(now)
	rt.Tick()

	st, _ := mgr.State("easee")
	in := evPlanCar(st, rt)
	if in.CapacityWh != 40000 || math.Abs(in.InitialSoC-.2) > 1e-9 || in.Source != "car:assumed" || in.ChargeLimit != 0 {
		t.Fatalf("plan inputs %+v, want the Volvo's", in)
	}
	if st.TargetSoC != .6 || st.TargetTime.IsZero() || loadpoint.PlanningTarget(st, in.ChargeLimit) != .6 {
		t.Fatalf("goal %v by %v, want the Volvo's 60 %%", st.TargetSoC, st.TargetTime)
	}
	minW, maxW, steps := loadpoint.LimitPower(st, st.AllowedStepsW)
	if maxW != 7400 || minW != 4140 || len(steps) != 2 || steps[1] != 7360 {
		t.Fatalf("power %v–%v %v, want the Volvo's own 7.4 kW", minW, maxW, steps)
	}
}
