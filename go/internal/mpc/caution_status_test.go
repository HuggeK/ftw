package mpc

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// A plan that falls back to the point forecast says so on the plan itself,
// not only in a warning string, and the next plan that keeps its caution
// clears it. A style that asks for no caution never reads as caution off.
func TestCautionOffIsPublishedAndClears(t *testing.T) {
	for _, tc := range []struct {
		name       string
		k          float64
		fault      string
		cautionOff bool
	}{
		{"caution kept", .3, "", false},
		{"caution infeasible", .3, "infeasible", true},
		{"caution inputs invalid", .3, "nan uncertainty", true},
		{"caution risk invalid", .3, "nan risk", true},
		{"no caution asked", 0, "infeasible", false},
		{"no caution asked with invalid inputs", 0, "nan uncertainty", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := shadowTestService(t)
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.FuseMaxW, svc.PVForecastSafetyK = 3000, tc.k
			fault := tc.fault
			svc.PVUncertaintyW = func() float64 {
				if fault == "nan uncertainty" {
					return math.NaN()
				}
				return 0
			}
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{Load: func(time.Time) float64 { return 1000 },
					PVUncertaintyW: svc.PVUncertaintyW(),
					Risk: func(_, risk []Slot, _ float64) {
						for i := range risk {
							switch fault {
							case "infeasible":
								// More house load than the fuse and battery can serve.
								risk[i].LoadW = 9000
							case "nan risk":
								risk[i].LoadW = math.NaN()
							default:
								risk[i].LoadW = 1100
							}
						}
					}}
			}
			plan := svc.Replan(context.Background())
			if plan == nil || len(plan.Actions) != 4 {
				t.Fatalf("no full plan: %+v", svc.PlanSnapshot())
			}
			if plan.CautionOff != tc.cautionOff {
				t.Fatalf("caution off = %v, want %v; warnings %q", plan.CautionOff, tc.cautionOff, svc.PlanSnapshot().Warnings)
			}
			if d := svc.Diagnose(); d == nil || d.CautionOff != tc.cautionOff {
				t.Fatal("diagnostic lost the caution state")
			}
			if tc.cautionOff && !strings.Contains(strings.Join(svc.PlanSnapshot().Warnings, " "), "caution") {
				t.Fatalf("no warning says why: %q", svc.PlanSnapshot().Warnings)
			}

			fault = ""
			if plan := svc.Replan(context.Background()); plan == nil || plan.CautionOff {
				t.Fatalf("working caution did not clear the flag: %+v", plan)
			}
		})
	}
}

// A caution failure that repeats on every replan is one record and one write.
// A changed message is new evidence. A hard failure that returns after a
// recovered plan is current again rather than marked recovered.
func TestRepeatedPlanningFailureIsWrittenOnce(t *testing.T) {
	svc := shadowTestService(t)
	start := time.Now().UTC().Truncate(time.Hour)
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	svc.PVForecastSafetyK = .3
	uncertainty := math.NaN()
	svc.PVUncertaintyW = func() float64 { return uncertainty }
	if svc.Replan(context.Background()) == nil {
		t.Fatal("no plan")
	}
	first := svc.PlanningFailureSnapshot()
	svc.mu.RLock()
	written := svc.persistedPlanningFailure
	svc.mu.RUnlock()
	if first == nil || first.Stage != "risk_inputs" || written == nil || first.RecoveredDecisionID == "" {
		t.Fatalf("caution failure not recorded and recovered: %+v", first)
	}
	if svc.Replan(context.Background()) == nil {
		t.Fatal("no plan")
	}
	svc.mu.RLock()
	same := svc.persistedPlanningFailure == written && svc.lastPlanningFailure == written
	svc.mu.RUnlock()
	if again := svc.PlanningFailureSnapshot(); !same || again.CapturedAtMs != first.CapturedAtMs {
		t.Fatal("a repeated failure was recorded and written again")
	}

	// NaN and -1 give the same message, so only another field is a change.
	uncertainty = -1
	if svc.Replan(context.Background()) == nil || svc.PlanningFailureSnapshot().Generation != first.Generation {
		t.Fatal("the same message was recorded again")
	}
	uncertainty = 0
	relative := math.NaN()
	svc.PVRelativeUncertainty = func() float64 { return relative }
	if svc.Replan(context.Background()) == nil {
		t.Fatal("no plan")
	}
	if changed := svc.PlanningFailureSnapshot(); changed.Error == first.Error || changed.Generation == first.Generation {
		t.Fatalf("a changed message kept the old evidence: %+v", changed)
	}

	// The same hard failure twice, with a recovered plan in between.
	relative = 0
	if err := svc.Store.SavePrices(nil); err != nil {
		t.Fatal(err)
	}
	svc.Zone = "SE4"
	svc.Replan(context.Background())
	hard := svc.PlanningFailureSnapshot()
	if hard == nil || hard.Stage != "inputs" || hard.RecoveredDecisionID != "" {
		t.Fatalf("hard failure not recorded: %+v", hard)
	}
	svc.Zone = "SE3"
	plan := svc.Replan(context.Background())
	if plan == nil || svc.PlanningFailureSnapshot().RecoveredDecisionID != plan.DecisionID {
		t.Fatal("recovery not marked")
	}
	svc.Zone = "SE4"
	svc.Replan(context.Background())
	if back := svc.PlanningFailureSnapshot(); back.Error != hard.Error || back.RecoveredDecisionID != "" {
		t.Fatalf("returning failure still reads as recovered: %+v", back)
	}
	svc.persistPlanningFailure()
	if !svc.PlanningFailureStorageSnapshot().Durable {
		t.Fatal("current failure was not written")
	}
}

// With time for both passes the caution plan is solved first. When it meets
// every departure no point plan can do better, so one solve publishes it.
// A missed departure needs the point plan to compare against, and so does a
// slot about to end, where caution must not use up the point plan's time.
func TestForecastCautionSolvesFirstWhenBothPassesFit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		target   float64
		leftover time.Duration
		order    string
		caution  bool
	}{
		{"battery only", 0, time.Hour, "caution", true},
		{"reachable departure", .3, time.Hour, "caution", true},
		{"missed departure", .99, time.Hour, "caution point", true},
		{"slot about to end", 0, 5 * time.Second, "point caution", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, p := externalTestFixture()
			p.SoCLevels, p.ActionLevels = 21, 11
			if tc.target > 0 {
				p.Loadpoint = &LoadpointSpec{ID: "car", CapacityWh: 10000, InitialSoC: .2, SoCMax: 1, Levels: 11,
					PluggedIn: true, TargetSoC: tc.target, TargetSlotIdx: 0, AllowedStepsW: []float64{0, 1000}, MaxChargeW: 1000,
					ChargeEfficiency: 1, NoBatteryToEV: true}
				p.Loadpoints = []*LoadpointSpec{p.Loadpoint}
			}
			risk := append([]Slot(nil), slots...)
			for i := range risk {
				risk[i].LoadW += 100
			}
			end := time.UnixMilli(slots[0].StartMs + int64(slots[0].LenMin)*60000)
			var order []string
			choice, err := planWithForecastMargin(context.Background(), slots, risk, p,
				func() time.Time { return end.Add(-tc.leftover) },
				func(ctx context.Context, in []Slot, p Params) (Plan, []Slot, error) {
					if in[0].LoadW == risk[0].LoadW {
						order = append(order, "caution")
					} else {
						order = append(order, "point")
					}
					return corePlanWithRecovery(ctx, in, p)
				})
			if err != nil || strings.Join(order, " ") != tc.order {
				t.Fatalf("solve order %q, want %q: %v", order, tc.order, err)
			}
			if got := choice.slots[0].LoadW == risk[0].LoadW; got != tc.caution || choice.withoutCaution {
				t.Fatalf("caution plan chosen = %v, want %v: %s", got, tc.caution, choice.warning)
			}
		})
	}
}

// The caution solve must end inside the slot after a slow point solve, and
// when caution goes first it leaves the point plan a full worker budget.
func TestForecastCautionBudgetEndsInsideTheSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		left time.Duration
		// The latest caution deadline, counted from the slot's end.
		latest time.Duration
	}{
		{"point first after a slow point solve", 2 * time.Second, energyplanPublishMargin},
		{"caution first", forecastRecoveryBudget + time.Second, energyplanPublishMargin + energyplanFleetBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, p := externalTestFixture()
			slots = slots[:1]
			p.SoCLevels, p.ActionLevels = 21, 11
			risk := append([]Slot(nil), slots...)
			risk[0].LoadW += 100
			began := time.Now()
			slotEnd := began.Add(tc.left)
			serviceEnd := time.UnixMilli(slots[0].StartMs + int64(slots[0].LenMin)*60000)
			now := func() time.Time { return serviceEnd.Add(-tc.left).Add(time.Since(began)) }
			var deadline time.Time
			_, err := planWithForecastMargin(context.Background(), slots, risk, p, now,
				func(ctx context.Context, in []Slot, p Params) (Plan, []Slot, error) {
					if in[0].LoadW == risk[0].LoadW {
						deadline, _ = ctx.Deadline()
					} else {
						time.Sleep(300 * time.Millisecond)
					}
					return corePlanWithRecovery(ctx, in, p)
				})
			if err != nil || deadline.IsZero() {
				t.Fatalf("no caution solve: %v", err)
			}
			if limit := slotEnd.Add(-tc.latest).Add(20 * time.Millisecond); deadline.After(limit) {
				t.Fatalf("caution may run until %v after the slot's last safe moment", deadline.Sub(slotEnd.Add(-tc.latest)))
			}
		})
	}
}

// A point plan that covers only part of the horizon never keeps caution, in
// either order, so the published plan says so.
func TestShortenedPointPlanIsWithoutCaution(t *testing.T) {
	for _, left := range []time.Duration{2 * time.Second, time.Hour} {
		t.Run(left.String(), func(t *testing.T) {
			slots, p := externalTestFixture()
			p.SoCLevels, p.ActionLevels = 21, 11
			risk := append([]Slot(nil), slots...)
			for i := range risk {
				risk[i].LoadW += 100
			}
			end := time.UnixMilli(slots[0].StartMs + int64(slots[0].LenMin)*60000)
			choice, err := planWithForecastMargin(context.Background(), slots, risk, p,
				func() time.Time { return end.Add(-left) },
				func(ctx context.Context, in []Slot, p Params) (Plan, []Slot, error) {
					if in[0].LoadW == risk[0].LoadW {
						return Plan{}, nil, errors.New("caution unavailable")
					}
					return corePlanWithRecovery(ctx, in[:1], p)
				})
			if err != nil || len(choice.slots) != 1 || !choice.withoutCaution {
				t.Fatalf("shortened point plan: slots=%d without caution=%v err=%v", len(choice.slots), choice.withoutCaution, err)
			}
		})
	}
}

// priorOnlyOptimizer finds every forecast infeasible except the configured
// load prior, so the service recovers with the prior.
type priorOnlyOptimizer struct {
	downside bool
	priorW   float64
}

func (o priorOnlyOptimizer) UsesDownsidePV() bool { return o.downside }
func (priorOnlyOptimizer) Close() error           { return nil }
func (o priorOnlyOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	if slots[len(slots)-1].LoadW != o.priorW {
		return Plan{}, &externalError{Code: "infeasible", Message: "test"}
	}
	plan, _, err := corePlanWithRecovery(ctx, slots, p)
	return plan, err
}

// Only a planner that solves the caution forecast loses it to a prior. A
// planner that never used caution does not read as caution off.
func TestPriorRecoveryMarksCautionOffOnlyForDownsidePlanners(t *testing.T) {
	for _, downside := range []bool{false, true} {
		t.Run(map[bool]string{false: "point planner", true: "downside planner"}[downside], func(t *testing.T) {
			svc := shadowTestService(t)
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.PVForecastSafetyK = .3
			svc.Optimizer = priorOnlyOptimizer{downside: downside, priorW: svc.BaseLoad}
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{Load: func(time.Time) float64 { return 1000 },
					Risk: func(_, risk []Slot, _ float64) {
						for i := range risk {
							risk[i].LoadW = 1200
						}
					}}
			}
			plan := svc.Replan(context.Background())
			svc.shadowWG.Wait()
			if plan == nil || plan.Solver == nil || plan.Solver.Fallback || svc.lastSlots[3].LoadW != svc.BaseLoad {
				t.Fatalf("no prior plan from the optimizer: %+v", svc.PlanSnapshot())
			}
			if plan.CautionOff != downside {
				t.Fatalf("caution off = %v, want %v", plan.CautionOff, downside)
			}
		})
	}
}

// A slot number or a watt figure that moves between replans does not make a
// lasting failure new evidence.
func TestRepeatedFailureIgnoresChangingNumbers(t *testing.T) {
	svc := shadowTestService(t)
	svc.lastPlanningFailure = &PlanningFailure{Stage: "forecast_caution", Error: "slot 6 grid_w -120.5 violates grid limits"}
	if !svc.repeatsPlanningFailure("forecast_caution", "slot 17 grid_w -98.25 violates grid limits") {
		t.Fatal("changed numbers made the same failure new")
	}
	if svc.repeatsPlanningFailure("forecast_caution", "slot 6 grid_w -120.5 violates mode") {
		t.Fatal("a different failure was taken for a repeat")
	}
	if svc.repeatsPlanningFailure("risk_inputs", "slot 6 grid_w -120.5 violates grid limits") {
		t.Fatal("a different stage was taken for a repeat")
	}
}
