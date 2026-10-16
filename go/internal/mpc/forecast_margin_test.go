package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type optionalMarginOptimizer struct {
	worker *ExternalOptimizer
	fault  string
	calls  int
}

func (*optionalMarginOptimizer) UsesDownsidePV() bool { return true }
func (*optionalMarginOptimizer) Close() error         { return nil }
func (o *optionalMarginOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls++
	if o.calls == 1 {
		return o.worker.Optimize(ctx, slots, p)
	}
	switch o.fault {
	case "infeasible":
		return Plan{}, &externalError{Code: "infeasible", Message: "optional margin"}
	case "broken reply":
		return Plan{}, errors.New("decode optimizer response: invalid JSON")
	case "timeout":
		<-ctx.Done()
		return Plan{}, ctx.Err()
	case "invalid plan":
		return Plan{Actions: []Action{{GridW: math.NaN()}}}, nil
	}
	return o.worker.Optimize(ctx, slots, p)
}

func TestForecastMarginKeepsPointAfterOptionalFailure(t *testing.T) {
	for _, fault := range []string{"infeasible", "broken reply", "timeout", "invalid plan"} {
		t.Run(fault, func(t *testing.T) {
			svc := shadowTestService(t)
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			worker := nativeWorker(t, time.Second)
			t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait(); _ = worker.Close() })
			optimizer := &optionalMarginOptimizer{worker: worker, fault: fault}
			svc.Optimizer = optimizer
			svc.Defaults.MaxChargeW, svc.Defaults.MaxDischargeW = 0, 0
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{Load: func(time.Time) float64 { return 1000 },
					Risk: func(_, risk []Slot, _ float64) {
						for i := range risk {
							risk[i].LoadW = 1200
						}
					}}
			}
			ctx := context.Background()
			if fault == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			plan := svc.Replan(ctx)
			svc.shadowWG.Wait()
			if plan == nil || len(plan.Actions) != 4 || plan.Solver.Engine != "native" || plan.Solver.Fallback || optimizer.calls != 2 {
				t.Fatalf("optional failure lost the native point plan: status=%+v calls=%d", svc.PlanSnapshot(), optimizer.calls)
			}
			if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
				t.Fatal(err)
			}
			for _, slot := range svc.lastSlots {
				if slot.LoadW != 1000 {
					t.Fatal("optional failure changed the point forecast")
				}
			}
			failure := svc.PlanningFailureSnapshot()
			if failure == nil || failure.RecoveredDecisionID != plan.DecisionID || !svc.PlanningFailureStorageSnapshot().Durable {
				t.Fatal("optional failure lost its durable evidence")
			}
		})
	}
}

func TestForecastMarginInvalidPVKeepsFullPointHorizon(t *testing.T) {
	for _, backend := range []string{"core", "native"} {
		for _, badSlot := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/slot%d", backend, badSlot), func(t *testing.T) {
				svc := shadowTestService(t)
				start := time.Now().UTC().Truncate(time.Hour)
				svc.now = func() time.Time { return start.Add(5 * time.Minute) }
				if backend == "native" {
					worker := nativeWorker(t, time.Second)
					t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait(); _ = worker.Close() })
					svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
				}
				svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
					return ForecastInputs{Load: func(time.Time) float64 { return 1000 },
						Risk: func(_, risk []Slot, _ float64) { risk[badSlot].PVW = math.NaN() }}
				}
				plan := svc.Replan(context.Background())
				svc.shadowWG.Wait()
				if plan == nil || len(plan.Actions) != 4 || len(svc.lastSlots) != 4 {
					t.Fatal("invalid optional PV removed a valid point interval")
				}
				failure := svc.PlanningFailureSnapshot()
				if failure == nil || !strings.Contains(string(failure.Inputs), `"NaN"`) || failure.RecoveredDecisionID != plan.DecisionID {
					t.Fatal("raw invalid risk was hidden")
				}
			})
		}
	}
}

func TestForecastMarginRetainsRealDepartureShortfall(t *testing.T) {
	for _, backend := range []string{"core", "native", "worker unavailable"} {
		t.Run(backend, func(t *testing.T) {
			svc := shadowTestService(t)
			if backend == "native" {
				worker := nativeWorker(t, time.Second)
				t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait(); _ = worker.Close() })
				svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
			} else if backend == "worker unavailable" {
				svc.Optimizer = &physicsGateFailingOptimizer{}
			}
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.Defaults.MaxChargeW, svc.Defaults.MaxDischargeW = 0, 0
			svc.FuseMaxW, svc.BaseLoad = 17250, 800
			svc.Loadpoint = func(int) *LoadpointSpec {
				return &LoadpointSpec{ID: "car", CapacityWh: 86500, InitialSoC: .68, SoCMax: 1,
					Levels: 101, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 0,
					AllowedStepsW: []float64{0, 4140, 6900, 10350}, MinChargeW: 4140, MaxChargeW: 11000, ChargeEfficiency: .9, NoBatteryToEV: true}
			}
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{Load: func(time.Time) float64 { return 12000 },
					Risk: func(_, risk []Slot, _ float64) {
						for i := range risk {
							risk[i].LoadW = 15000
						}
					}}
			}
			plan := svc.Replan(context.Background())
			svc.shadowWG.Wait()
			want := 10380 - 4140*(55.0/60)*.9
			if plan == nil || len(plan.Actions) != 4 || math.Abs(plan.LoadpointShortfallWh["car"]-want) > 1 || plan.Actions[0].LoadpointW != 4140 {
				t.Fatalf("unreachable goal changed the real shortfall: %+v", svc.PlanSnapshot())
			}
			if svc.lastSlots[0].LoadW != 12000 {
				t.Fatal("unreachable goal triggered a fictitious load prior")
			}
		})
	}
}

func TestForecastMarginCannotTradeDepartureGoalsBetweenCars(t *testing.T) {
	slots, p := externalTestFixture()
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	p.Loadpoints = nil
	for _, id := range []string{"first", "second"} {
		p.Loadpoints = append(p.Loadpoints, &LoadpointSpec{ID: id, CapacityWh: 10000, InitialSoC: .2, SoCMax: 1, Levels: 11,
			PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 1, AllowedStepsW: []float64{0, 2000}, MaxChargeW: 2000, ChargeEfficiency: 1})
	}
	for i := range slots {
		slots[i].PVW, slots[i].LoadW = 0, 1000
		slots[i].Limits = PowerLimits{MaxImportW: 10000, MaxExportW: 10000}
	}
	risk := append([]Slot(nil), slots...)
	for i := range risk {
		risk[i].LoadW = 1200
	}
	calls := 0
	var point, candidate Plan
	choice, err := planWithForecastMargin(context.Background(), slots, risk, p, func() time.Time { return time.UnixMilli(slots[0].ExecutionStart()) }, func(_ context.Context, inputs []Slot, p Params) (Plan, []Slot, error) {
		calls++
		powers := [][2]float64{{0, 2000}, {2000, 2000}}
		if calls == 2 {
			powers = [][2]float64{{2000, 2000}, {2000, 1000}}
		}
		plan := Plan{CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC, HorizonSlots: 2, Mode: p.Mode}
		soc := [2]float64{.2, .2}
		for i, slot := range inputs {
			a := Action{SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin, SoC: p.InitialSoC,
				LoadW: slot.LoadW, LoadpointPowerW: map[string]float64{}, LoadpointSoCByID: map[string]float64{}, LoadpointMaxPowerW: map[string]float64{}}
			for j, lp := range p.Loadpoints {
				w := powers[i][j]
				soc[j] += w * slot.DurationHours() / 10000
				a.LoadpointPowerW[lp.ID], a.LoadpointSoCByID[lp.ID] = w, soc[j]
				a.LoadpointMaxPowerW[lp.ID] = 0
				if w > 0 {
					a.LoadpointMaxPowerW[lp.ID] = 2000
				}
				a.GridW += w
			}
			a.GridW += slot.LoadW
			a.LoadpointW, a.LoadpointSoC = powers[i][0], soc[0]
			a.CostOre = reserveSlotCost(slot, p, a)
			plan.TotalCostOre += a.CostOre
			plan.Actions = append(plan.Actions, a)
		}
		if err := ValidatePlan(inputs, p, &plan); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			point = plan
		} else {
			candidate = plan
		}
		return plan, inputs, nil
	})
	if err != nil || calls != 2 || choice.warning == "" || choice.slots[0].LoadW != 1000 {
		t.Fatalf("risk traded one car's departure for another: %+v %v", choice, err)
	}
	if candidate.LoadpointShortfallWh["first"]+candidate.LoadpointShortfallWh["second"] >= point.LoadpointShortfallWh["first"]+point.LoadpointShortfallWh["second"] {
		t.Fatal("fixture must improve total shortfall while worsening the second car")
	}
	before, _ := json.Marshal(point)
	after, _ := json.Marshal(choice.plan)
	if string(before) != string(after) {
		t.Fatal("point plan changed when risk was rejected")
	}
}

func TestForecastMarginCannotGrantExtraDischargePermission(t *testing.T) {
	for _, mode := range []Mode{ModeSelfConsumption, ModeArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			slots, p := externalTestFixture()
			slots = slots[:1]
			slots[0].LoadW, p.Mode, p.InitialSoC = 2000, mode, .5
			p.DischargeEfficiency = 1
			a := Action{SlotStartMs: slots[0].StartMs, SlotLenMin: 60, LoadW: 2000, BatteryW: -1500, GridW: 500, SoC: .35}
			if mode == ModeArbitrage {
				p.Loadpoint = &LoadpointSpec{ID: "car", CapacityWh: 10000, InitialSoC: .2, SoCMax: 1, Levels: 11, PluggedIn: true,
					TargetSoC: .3, TargetSlotIdx: 0, MaxChargeW: 1000, AllowedStepsW: []float64{0, 1000}, ChargeEfficiency: 1, NoBatteryToEV: true}
				a.LoadpointW, a.LoadpointSoC, a.GridW = 1000, .3, 1500
			}
			a.CostOre = reserveSlotCost(slots[0], p, a)
			plan := Plan{Mode: mode, CapacityWh: p.CapacityWh, InitialSoC: .5, HorizonSlots: 1, Actions: []Action{a}, TotalCostOre: a.CostOre}
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatalf("risk candidate must satisfy its inflated load: %v", err)
			}
			point := append([]Slot(nil), slots...)
			point[0].LoadW = 1000
			before, _ := json.Marshal(plan)
			if err := validateMarginPointLoad(point, slots, p, plan); err == nil {
				t.Fatal("optional load granted battery export or battery-to-EV permission")
			}
			after, _ := json.Marshal(plan)
			if string(before) != string(after) {
				t.Fatal("diagnostic replay changed the candidate")
			}
		})
	}
}

func TestForecastMarginOnlyUsesTimeLeftAfterPointPlan(t *testing.T) {
	slots, p := externalTestFixture()
	slots = slots[:1]
	slots[0].ExecutionStartMs = slots[0].StartMs + 3600000 - 1
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	risk := append([]Slot(nil), slots...)
	risk[0].LoadW += 100
	calls := 0
	choice, err := planWithForecastMargin(context.Background(), slots, risk, p, func() time.Time { return time.UnixMilli(slots[0].ExecutionStart()) }, func(ctx context.Context, inputs []Slot, p Params) (Plan, []Slot, error) {
		calls++
		plan, err := coreReservePlan(ctx, inputs, p)
		return plan, inputs, err
	})
	if err != nil || calls != 1 || choice.warning == "" || choice.slots[0].LoadW != slots[0].LoadW {
		t.Fatalf("optional risk spent the point plan's publication time: calls=%d err=%v warning=%s", calls, err, choice.warning)
	}
}

func TestForecastMarginRetainsCautionWhenGoalsAndLimitsAllowIt(t *testing.T) {
	slots, p := externalTestFixture()
	p.SoCLevels, p.ActionLevels = 11, 5
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	risk := append([]Slot(nil), slots...)
	for i := range risk {
		risk[i].LoadW += 100
	}
	choice, err := planWithForecastMargin(context.Background(), slots, risk, p, func() time.Time { return time.UnixMilli(slots[0].ExecutionStart()) }, corePlanWithRecovery)
	if err != nil || choice.warning != "" || choice.slots[0].LoadW != risk[0].LoadW {
		t.Fatalf("valid optional caution was discarded: %v %s", err, choice.warning)
	}
}

func TestForecastMarginCannotChangeSiteOrMarketInputs(t *testing.T) {
	slots, p := externalTestFixture()
	p.SoCLevels, p.ActionLevels = 11, 5
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	risk := append([]Slot(nil), slots...)
	risk[0].LoadW += 100
	risk[0].Limits.MaxImportW += 1000
	calls := 0
	choice, err := planWithForecastMargin(context.Background(), slots, risk, p, func() time.Time { return time.UnixMilli(slots[0].ExecutionStart()) }, func(ctx context.Context, inputs []Slot, p Params) (Plan, []Slot, error) {
		calls++
		return corePlanWithRecovery(ctx, inputs, p)
	})
	if err != nil || calls != 1 || choice.warning == "" || choice.slots[0].Limits != slots[0].Limits {
		t.Fatalf("optional forecast changed the site limits: %v %+v", err, choice)
	}
}

func TestForecastMarginKeepsActualPublicationTime(t *testing.T) {
	slots, p := externalTestFixture()
	slots = slots[:1]
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	end := time.Now().Add(150 * time.Millisecond).UnixMilli()
	// This snapshot was frozen 850 ms ago; 1 s was then available.
	slots[0].StartMs = end - 3600000
	slots[0].LenMin = 60
	slots[0].ExecutionStartMs = end - 1000
	slots[0].PVW, slots[0].LoadW = 0, 1000
	risk := append([]Slot(nil), slots...)
	risk[0].LoadW += 100
	calls := 0
	began := time.Now()
	choice, err := planWithForecastMargin(context.Background(), slots, risk, p, time.Now, func(ctx context.Context, inputs []Slot, p Params) (Plan, []Slot, error) {
		calls++
		if calls == 2 {
			<-ctx.Done()
			return Plan{}, nil, ctx.Err()
		}
		plan, err := coreReservePlan(ctx, inputs, p)
		return plan, inputs, err
	})
	if err != nil || choice.warning == "" || len(choice.plan.Actions) != 1 {
		t.Fatalf("fixture did not retain its point plan: %v %+v", err, choice)
	}
	elapsed := time.Since(began)
	if time.Now().UnixMilli() >= end {
		t.Fatalf("optional risk spent the point plan's actual publication time: elapsed=%v; only 150ms remained at entry, frozen duration=1s, calls=%d", elapsed, calls)
	}
}

// These fields describe optional caution, not equipment. Invalid values must
// survive in support evidence while the effective solve uses the point model.
func TestForecastMarginBadOptionalParametersKeepPoint(t *testing.T) {
	for _, backend := range []string{"core", "native"} {
		for _, field := range []string{"PVUncertaintyW", "PVRelativeUncertainty", "PVForecastSafetyK"} {
			for _, bad := range []struct {
				name string
				v    float64
				raw  any
			}{{"NaN", math.NaN(), "NaN"}, {"positive infinity", math.Inf(1), "+Inf"}, {"negative infinity", math.Inf(-1), "-Inf"}, {"negative", -1, float64(-1)}} {
				t.Run(backend+"/"+field+"/"+bad.name, func(t *testing.T) {
					svc := shadowTestService(t)
					start := time.Now().UTC().Truncate(time.Hour)
					svc.now = func() time.Time { return start.Add(5 * time.Minute) }
					svc.Defaults.MaxChargeW, svc.Defaults.MaxDischargeW = 0, 0
					svc.PVForecastSafetyK = 1
					svc.PV = func(time.Time, float64) float64 { return 1000 }
					if backend == "native" {
						worker := nativeWorker(t, time.Second)
						t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait(); _ = worker.Close() })
						svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
					}
					switch field {
					case "PVUncertaintyW":
						svc.PVUncertaintyW = func() float64 { return bad.v }
					case "PVRelativeUncertainty":
						svc.PVRelativeUncertainty = func() float64 { return bad.v }
					case "PVForecastSafetyK":
						svc.PVForecastSafetyK = bad.v
					}
					plan := svc.Replan(context.Background())
					svc.shadowWG.Wait()
					if plan == nil || len(plan.Actions) != 4 || len(svc.PlanSnapshot().Warnings) == 0 {
						t.Fatalf("optional %s blocked the point plan: %+v", field, svc.PlanSnapshot())
					}
					if svc.lastParams.PVUncertaintyW != 0 || svc.lastParams.PVRelativeUncertainty != 0 || svc.lastParams.PVForecastSafetyK != 0 {
						t.Fatal("invalid caution remained in effective planning parameters")
					}
					failure := svc.PlanningFailureSnapshot()
					var inputs struct {
						Params map[string]any `json:"params"`
					}
					if failure == nil || json.Unmarshal(failure.Inputs, &inputs) != nil || inputs.Params[field] != bad.raw ||
						failure.RecoveredDecisionID != plan.DecisionID || !svc.PlanningFailureStorageSnapshot().Durable {
						t.Fatalf("raw %s was lost before disabling caution: %+v", field, failure)
					}
				})
			}
		}
	}
}
