package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Reproduce the reported chain: prices exist, one impossible future forecast
// used to fail the native optimiser and truncate the reserve before cheap hours.
func TestNativePlannerRetainsPricesAndHorizonAfterBadForecast(t *testing.T) {
	for _, fault := range []string{"primary", "legacy", "both", "legacy only", "nan load", "infinite load", "negative load", "both nan", "huge load", "risk nan load", "risk load", "risk energy"} {
		t.Run(fault, func(t *testing.T) {
			svc := shadowTestService(t)
			worker := nativeWorker(t, 500*time.Millisecond)
			t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait(); _ = worker.Close() })
			svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.FuseMaxW, svc.BaseLoad, svc.PVForecastSafetyK = 17250, 800, 1
			svc.Defaults.Mode = ModeArbitrage
			svc.Defaults.MaxChargeW, svc.Defaults.MaxDischargeW = 0, 0
			svc.Loadpoint = func(int) *LoadpointSpec {
				return &LoadpointSpec{ID: "car", CapacityWh: 10000, InitialSoC: .2, SoCMax: 1, PluggedIn: true,
					TargetSoC: .8, TargetSlotIdx: 3, Levels: 11, MaxChargeW: 11000, MinChargeW: 4140,
					AllowedStepsW: []float64{0, 4140, 6900, 11000}, ChargeEfficiency: .9, NoBatteryToEV: true}
			}
			for i := 0; i < 4; i++ {
				price := 450.0
				if i >= 2 {
					price = 50
				}
				if err := svc.Store.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * time.Hour).UnixMilli(), SlotLenMin: 60, SpotOreKwh: price, TotalOreKwh: price, Source: "test", FetchedAtMs: start.UnixMilli()}}); err != nil {
					t.Fatal(err)
				}
			}
			var archivedBase, archivedPlanning []Slot
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{
					Load: func(at time.Time) float64 {
						if at.Equal(start.Add(2*time.Hour)) && (fault == "legacy" || fault == "both" || fault == "legacy only") {
							return 55700
						}
						if fault == "both nan" {
							return math.NaN()
						}
						return 1800
					},
					Resolve: func(_ context.Context, slots []Slot) []Slot {
						if fault == "legacy only" {
							return slots
						}
						for i := range slots {
							slots[i].LoadW = 1700
						}
						switch fault {
						case "primary", "both":
							slots[2].LoadW = 55700
						case "legacy":
							slots[2].LoadW = 1800 // this primary is healthy
						case "negative load":
							slots[2].LoadW = -1000
						case "huge load":
							slots[2].LoadW = math.MaxFloat64
						case "nan load", "both nan":
							slots[2].LoadW = math.NaN()
						case "infinite load":
							slots[2].LoadW = math.Inf(1)
						}
						return slots
					},
					Risk: func(_ []Slot, slots []Slot, _ float64) {
						switch fault {
						case "risk nan load":
							slots[2].LoadW = math.NaN()
						case "risk load":
							slots[2].LoadW = 55700
						case "risk energy":
							for i := range slots {
								slots[i].LoadW = 18000
							}
						case "risk nan pv":
							slots[0].PVW = math.NaN()
						}
					},
					Record: func(base, planning []Slot, _ []bool, _ string, _ int64) {
						archivedBase = append([]Slot(nil), base...)
						archivedPlanning = append([]Slot(nil), planning...)
					},
				}
			}
			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver == nil || plan.Solver.Engine != "native" || plan.Solver.Fallback || len(plan.Actions) != 4 {
				t.Fatalf("forecast failure disabled native price planning: solver=%+v status=%+v", plan.Solver, svc.PlanSnapshot())
			}
			if plan.Actions[0].LoadpointW > 1e-6 || plan.Actions[1].LoadpointW > 1e-6 || math.Abs(plan.Actions[3].LoadpointSoC-.8) > 1e-5 {
				t.Fatalf("lost cheap charging or departure goal: %+v", plan.Actions)
			}
			var request externalRequest
			if err := json.Unmarshal(plan.OptimizerInput, &request); err != nil {
				t.Fatal(err)
			}
			for i, slot := range svc.lastSlots {
				if slot.Limits.MaxImportW != 17250 || slot.StartMs != start.Add(time.Duration(i)*time.Hour).UnixMilli() || slot.PriceOre != request.Slots[i].PriceOre {
					t.Fatalf("forecast recovery changed market or limits: %+v", slot)
				}
				if slot.LoadW > 1800 || !finite(slot.LoadW) || slot.LoadW != archivedPlanning[i].LoadW || archivedBase[i].LoadW > 1800 {
					t.Fatalf("bad forecast reached native solver/archive: %+v", slot)
				}
			}
			if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
				t.Fatal(err)
			}
			if fault == "legacy" {
				if len(svc.PlanSnapshot().Warnings) != 0 {
					t.Fatal("healthy primary inherited rejected legacy values")
				}
			} else {
				failure := svc.PlanningFailureSnapshot()
				if failure == nil || failure.RecoveredDecisionID != plan.DecisionID || !svc.PlanningFailureStorageSnapshot().Durable {
					t.Fatalf("rejected raw forecast was not retained durably: %+v", failure)
				}
				if len(svc.PlanSnapshot().Warnings) == 0 || strings.Contains(strings.Join(svc.PlanSnapshot().Warnings, " "), "Plan ends at") {
					t.Fatal("forecast recovery was hidden or reported as a shortened plan")
				}
			}
		})
	}
}

func TestForecastRecoveryCannotChangeMeasuredOverload(t *testing.T) {
	slots, p := recoveryFixture()
	slots[0].LoadW = 55700
	prior := priorForecast(slots, 800, true)
	if prior[0].LoadW != 55700 || prior[1].LoadW != 800 || slots[1].LoadW != 2000 {
		t.Fatal("lost actual overload or mutated raw inputs")
	}
	choice, err := corePlanWithForecastRecovery(context.Background(), slots, slots, p, 800, true, time.Now)
	plan, used := choice.plan, choice.slots
	if err == nil || len(used) != 0 || len(plan.Actions) != 0 {
		t.Fatal("forecast prior hid a real current overload")
	}
}

func TestForecastRecoveryKeepsValidBatteryAndPVPeaks(t *testing.T) {
	for _, kind := range []string{"stored energy", "advance charging", "PV"} {
		t.Run(kind, func(t *testing.T) {
			slots, p := recoveryFixture()
			p.Loadpoint = nil
			switch kind {
			case "stored energy":
				slots[12].LoadW = 19000
				p.Mode = ModeSelfConsumption
			case "advance charging":
				slots[12].LoadW = 19000
				p.InitialSoC = p.SoCMin
			case "PV":
				slots[12].LoadW = 30000
				slots[12].PVW = -16000
				p.Mode = ModeSelfConsumption
			}
			choice, err := corePlanWithForecastRecovery(context.Background(), slots, slots, p, 800, false, time.Now)
			plan, used, warning := choice.plan, choice.slots, choice.warning
			if err != nil || warning != "" || len(used) != len(slots) || used[12].LoadW != slots[12].LoadW {
				t.Fatalf("valid peak was replaced: %v %s", err, warning)
			}
			assertRecoveryPlan(t, used, p, plan)
		})
	}
}

func TestForecastRecoveryOnlyHandlesStructuredInfeasibility(t *testing.T) {
	for _, code := range []string{"infeasible", "invalid_request", "numerical_failure", "deadline_exceeded", "cancelled"} {
		err := &optimizerFailureError{cause: &externalError{Code: code, Message: "infeasible"}}
		if forecastInfeasible(err) != (code == "infeasible") {
			t.Fatal("error message changed retry policy:", code)
		}
	}
	if forecastInfeasible(fmt.Errorf("optimizer infeasible: untrusted text")) || forecastInfeasible(context.DeadlineExceeded) {
		t.Fatal("text/timeout was treated as a forecast verdict")
	}
	slots, _ := recoveryFixture()
	base := append([]Slot(nil), slots...)
	slots[2].LoadW = 55700
	attempts := forecastRecoveryAttempts(slots, base, 800, false)
	if len(attempts) != 2 {
		t.Fatalf("unbounded or lost recovery attempts: %d", len(attempts))
	}
	for _, attempt := range attempts {
		for i, slot := range attempt.slots {
			if slot.StartMs != slots[i].StartMs || slot.LenMin != slots[i].LenMin || slot.PriceOre != slots[i].PriceOre || slot.SpotOre != slots[i].SpotOre || slot.Limits != slots[i].Limits {
				t.Fatal("recovery changed market, timeline or limits")
			}
		}
	}
}

// The archive learns which published loads Core replaced. The live meter's
// in-flight load is a measurement, so a valid meter alone marks nothing.
func TestRecordMarksOnlyRecoveredLoads(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "live meter", true: "invalid model load"}[invalid], func(t *testing.T) {
			svc := shadowTestService(t)
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.HouseholdMeasurement = func() telemetry.ForecastReading {
				return telemetry.ForecastReading{HouseholdW: 2345, Valid: true}
			}
			var base []Slot
			var recovered []bool
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{
					Resolve: func(_ context.Context, slots []Slot) []Slot {
						for i := range slots {
							slots[i].LoadW = 600 + 100*float64(i)
						}
						if invalid {
							slots[2].LoadW = math.NaN()
						}
						return slots
					},
					Record: func(b, _ []Slot, r []bool, _ string, _ int64) {
						base, recovered = append([]Slot(nil), b...), append([]bool(nil), r...)
					},
				}
			}
			if plan := svc.Replan(context.Background()); plan == nil {
				t.Fatalf("no plan: %+v", svc.PlanSnapshot())
			}
			want := []bool{false, false, invalid, false}
			if len(base) != 4 || base[0].LoadW != 2345 || fmt.Sprint(recovered) != fmt.Sprint(want) {
				t.Fatalf("recovered=%v want %v; base=%+v", recovered, want, base)
			}
		})
	}
}
