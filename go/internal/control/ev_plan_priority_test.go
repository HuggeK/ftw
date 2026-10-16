package control

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestScheduledEVKeepsBudgetBeforeBatteryCharge(t *testing.T) {
	for _, draw := range []float64{0, 4000, 8000} {
		t.Run(fmt.Sprint(draw), func(t *testing.T) {
			now := time.Now()
			dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 1250, LoadpointEnergyWh: map[string]float64{"garage": 2000}, Strategy: "arbitrage"}
			store := seedStore(200+draw, []struct {
				name          string
				currentW, soc float64
			}{{"ferroamp", 0, .5}})
			st := newStateWithEnergyDispatch(dir, "ferroamp")
			st.EVChargingW = draw
			st.BatteryCoversEV = true
			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
			if len(targets) != 1 {
				t.Fatalf("targets %v", targets)
			}
			if !st.FuseSaturated || math.Abs(st.FuseEVMaxW-8000) > 1 {
				t.Fatalf("EV budget lost while drawing %.0f: cap %.0f saturated %v", draw, st.FuseEVMaxW, st.FuseSaturated)
			}
			if targets[0].TargetW < 0 || targets[0].TargetW > 2800.1 {
				t.Fatalf("optional battery charge exceeds remaining 2800 W: %v", targets)
			}
			if 200+st.FuseEVMaxW+targets[0].TargetW > 11000.1 {
				t.Fatal("planned EV and battery exceed fuse")
			}
		})
	}
}

func TestScheduledEVRejectsExpiredDemand(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{SlotStart: now.Add(-30 * time.Minute), SlotEnd: now.Add(-15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 2000}}
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	if got := scheduledEVPowerW(st); got != 0 {
		t.Fatalf("stale demand reserved %.0f W", got)
	}
}

func TestScheduledEVReservesOnPowerNotMean(t *testing.T) {
	now := time.Now()
	d := mpc.SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		BatteryEnergyWh:    1250,
		GridW:              10600,
		LoadpointEnergyWh:  map[string]float64{"easee": 100},
		LoadpointMaxPowerW: map[string]float64{"easee": 10350},
		Strategy:           mpc.ModeArbitrage,
	}
	st := newStateWithEnergyDispatch(SlotDirectiveFromMPC(d), "ferroamp")
	if got := scheduledEVPowerW(st); got != 10350 {
		t.Fatalf("10350 W EV pulse with 100 Wh budget reserved %.0f W", got)
	}
	store := seedStore(200, []struct {
		name          string
		currentW, soc float64
	}{{"ferroamp", 0, .5}})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
	if len(targets) != 1 {
		t.Fatalf("targets %v", targets)
	}
	if !st.FuseSaturated || math.Abs(st.FuseEVMaxW-10350) > 1 {
		t.Fatalf("on-power not reserved: cap %.0f saturated %v", st.FuseEVMaxW, st.FuseSaturated)
	}
	if targets[0].TargetW < 0 || targets[0].TargetW > 450.1 {
		t.Fatalf("optional battery charge took EV on-power: %v", targets)
	}
}

func TestScheduledEVSumsSimultaneousOnPower(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh:  map[string]float64{"a": 50, "b": 80},
		LoadpointMaxPowerW: map[string]float64{"a": 4140, "b": 6900},
		Strategy:           "arbitrage",
	}, "ferroamp")
	if got := scheduledEVPowerW(st); got != 11040 {
		t.Fatalf("simultaneous pulses reserved %.0f W, want 11040", got)
	}
}

func TestScheduledEVFallsBackToMeanWithoutOnPower(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{
		SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2000},
		Strategy:          "arbitrage",
	}, "ferroamp")
	if got := scheduledEVPowerW(st); math.Abs(got-8000) > 1e-6 {
		t.Fatalf("mean reservation = %.0f W, want 8000", got)
	}
}

func TestChargerOfferReservesFuseBeforeOptionalBatteryCharge(t *testing.T) {
	for _, mode := range []Mode{ModeSelfConsumption, ModePlannerSelf, ModePlannerArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Now()
			dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Minute), EVOnly: true,
				LoadpointEnergyWh: map[string]float64{"manual": 11000.0 / 60}, LoadpointMaxPowerW: map[string]float64{"manual": 11000}}
			store := seedStore(-6000, []struct {
				name          string
				currentW, soc float64
			}{{"battery", 0, .5}})
			st := newStateWithEnergyDispatch(dir, "ferroamp")
			st.Mode = mode
			st.EVCommandedW = 11000 // Final manual/reserve offer, still 0 W measured.
			if got := scheduledEVPowerW(st); got != 11000 {
				t.Fatalf("ramping offer lost in %s: %v", mode, got)
			}
			targets := ComputeDispatch(store, st, caps(map[string]float64{"battery": 10000}), 5000)
			if len(targets) != 1 || targets[0].TargetW > 0.001 || (st.FuseSaturated && st.FuseEVMaxW < 10999) {
				t.Fatalf("battery took the EV's offered room: targets=%v evcap=%v", targets, st.FuseEVMaxW)
			}
			if mode.IsPlannerMode() && !st.PlanStale {
				t.Fatal("EV-only directive claimed a fresh battery plan")
			}
		})
	}
}

func TestBatterylessGoalIntentPublishesJointFuseBudget(t *testing.T) {
	now := time.Now()
	st := newStateWithEnergyDispatch(SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Minute), EVOnly: true,
		LoadpointEnergyWh: map[string]float64{"car": 11000.0 / 60}, LoadpointMaxPowerW: map[string]float64{"car": 11000}}, "ferroamp")
	st.EVCommandedW = 11000
	store := seedStore(4000, nil)
	if targets := ComputeDispatch(store, st, nil, 10000); len(targets) != 0 {
		t.Fatalf("invented a battery: %v", targets)
	}
	if !st.FuseSaturated || st.FuseEVMaxW > 6000.001 || st.FuseEVMaxW <= 0 {
		t.Fatalf("no-battery cap=%v saturated=%v", st.FuseEVMaxW, st.FuseSaturated)
	}
}
