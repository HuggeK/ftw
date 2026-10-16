package mpc

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// An unreachable departure changes the reported shortfall, never whether the
// rest of the site receives a plan. These expectations use direct Wh arithmetic.
func TestNativeEVGoalPlanAndPriceContract(t *testing.T) {
	for _, backend := range []string{"native", "reserve"} {
		t.Run(backend, func(t *testing.T) {
			solve := coreReservePlan
			if backend == "native" {
				worker := nativeWorker(t, time.Second)
				t.Cleanup(func() { _ = worker.Close() })
				solve = worker.Optimize
			}
			for _, tc := range []struct {
				name          string
				due, cheap    int
				houseW        float64
				partial       bool
				wantShortfall float64
				wantCostlyWh  float64
			}{
				{"enough cheaper hours", 15, 8, 1800, false, 0, 0},
				{"expensive energy needed for goal", 15, 12, 1800, false, 0, 10380.0/.9 - 10350},
				{"unreachable departure", 0, 8, 1800, false, 10380 - 10350*.25*.9, 10350 * .25},
				{"forty-five seconds to departure", 0, 8, 1800, true, 10380 - 10350*.0125*.9, 10350 * .0125},
				{"no legal charger step fits", 15, 8, 14000, false, 10380, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					start := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
					slots := make([]Slot, 16)
					for i := range slots {
						price := 450.0
						if i >= tc.cheap {
							price = 50
						}
						slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15,
							LoadW: tc.houseW, PriceOre: price, SpotOre: price,
							Limits: PowerLimits{MaxImportW: 17250, MaxExportW: 17250}}
					}
					if tc.partial {
						slots[0].ExecutionStartMs = slots[0].StartMs + (14*time.Minute + 15*time.Second).Milliseconds()
					}
					p := Params{Mode: ModeArbitrage, CapacityWh: 15000, InitialSoC: .5,
						SoCMin: .1, SoCMax: .9, SoCLevels: 101, ActionLevels: 201,
						ChargeEfficiency: .95, DischargeEfficiency: .95,
						Loadpoint: &LoadpointSpec{ID: "car", CapacityWh: 86500, InitialSoC: .68,
							SoCMax: 1, Levels: 101, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: tc.due,
							MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: chargerSteps(690),
							ChargeEfficiency: .9, NoBatteryToEV: true}}
					// Use three-phase steps from 6 to 15 A, up to 10,350 W.
					p.Loadpoint.AllowedStepsW = p.Loadpoint.AllowedStepsW[:len(p.Loadpoint.AllowedStepsW)-1]
					before, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := solve(context.Background(), slots, p)
					if err != nil {
						t.Fatalf("EV goal disabled site planning: %v", err)
					}
					if len(plan.Actions) != len(slots) || plan.HorizonSlots != len(slots) {
						t.Fatalf("EV goal shortened the site plan: %d actions, horizon %d", len(plan.Actions), plan.HorizonSlots)
					}
					if err := ValidatePlan(slots, p, &plan); err != nil {
						t.Fatal(err)
					}
					if got := plan.LoadpointShortfallWh["car"]; math.Abs(got-tc.wantShortfall) > 1 {
						t.Fatalf("shortfall %.6f Wh, want %.6f", got, tc.wantShortfall)
					}
					var deliveredWh, costlyWh float64
					for i, a := range plan.Actions {
						deliveredWh += a.LoadpointW * slots[i].DurationHours()
						if i < tc.cheap {
							costlyWh += a.LoadpointW * slots[i].DurationHours()
						}
						if i > tc.due && a.LoadpointW > 1e-6 {
							t.Fatalf("slot %d charges after departure: %.6f W", i, a.LoadpointW)
						}
						if math.Abs(a.SoC-p.InitialSoC) > 1e-6 || math.Abs(a.BatteryW) > 1e-6 {
							t.Fatal("EV goal changed the fixed battery trajectory")
						}
					}
					if math.Abs(deliveredWh*.9+tc.wantShortfall-10380) > 1 || math.Abs(costlyWh-tc.wantCostlyWh) > 1 {
						t.Fatalf("delivered %.6f Wh, expensive %.6f Wh; want %.6f and %.6f", deliveredWh, costlyWh, (10380-tc.wantShortfall)/.9, tc.wantCostlyWh)
					}
					if got := plan.Actions[tc.due].LoadpointSoC*86500 + tc.wantShortfall; math.Abs(got-.8*86500) > 1 {
						t.Fatalf("departure energy plus missing energy %.6f Wh, want 69200", got)
					}
					after, err := json.Marshal(p)
					if err != nil || string(before) != string(after) {
						t.Fatal("planner changed the requested goal or physical settings")
					}
				})
			}
		})
	}
}
