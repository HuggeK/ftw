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

func recoveryFixture() ([]Slot, Params) {
	p := Params{Mode: ModeArbitrage, CapacityWh: 15000, InitialSoC: .856, SoCMin: .1, SoCMax: .95,
		SoCLevels: 21, ActionLevels: 21, MaxChargeW: 10000, MaxDischargeW: 10000,
		ChargeEfficiency: .95, DischargeEfficiency: .95}
	p.Loadpoint = &LoadpointSpec{ID: "easee", CapacityWh: 86500, InitialSoC: .67, SoCMin: 0, SoCMax: .8,
		Levels: 11, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 11, MinChargeW: 4140, MaxChargeW: 11000,
		AllowedStepsW:    []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350},
		ChargeEfficiency: .9, NoBatteryToEV: true}
	start := time.Now().UTC().Truncate(15 * time.Minute).Add(15 * time.Minute)
	slots := make([]Slot, 24)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15,
			LoadW: 2000, PriceOre: 300, SpotOre: 160, Limits: PowerLimits{MaxImportW: 17250, MaxExportW: 17250}}
	}
	return slots, p
}

func TestCoreReservePreparesBatteryForFuturePeaks(t *testing.T) {
	for _, peak := range []string{"import", "export"} {
		t.Run(peak, func(t *testing.T) {
			slots, p := recoveryFixture()
			if peak == "import" {
				p.InitialSoC = p.SoCMin
				slots[12].LoadW = 19000
			} else {
				p.InitialSoC = p.SoCMax
				slots[12].PVW = -21000
			}
			plan := mustCoreReservePlan(t, slots, p)
			if len(plan.Actions) != len(slots) {
				t.Fatalf("lost a feasible horizon: %d actions", len(plan.Actions))
			}
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatal(err)
			}
			before := plan.Actions[11].SoC
			if (peak == "import" && before <= p.InitialSoC) || (peak == "export" && before >= p.InitialSoC) {
				t.Fatalf("battery not prepared: initial=%g before peak=%g", p.InitialSoC, before)
			}
		})
	}
}

func TestCoreRecoveryKeepsSafePrefixBeforeImpossibleForecast(t *testing.T) {
	for _, ev := range []bool{false, true} {
		for _, peak := range []string{"import", "export"} {
			t.Run(peak+map[bool]string{true: "/ev", false: "/battery"}[ev], func(t *testing.T) {
				slots, p := recoveryFixture()
				if !ev {
					p.Loadpoint = nil
				} else {
					p.Loadpoint.TargetSlotIdx = 20 // beyond the shortened plan
				}
				if peak == "import" {
					slots[12].LoadW = 40000
				} else {
					slots[12].PVW = -40000
				}
				plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
				if err != nil {
					t.Fatal(err)
				}
				if len(used) != 12 || len(plan.Actions) != 12 || plan.HorizonSlots != 12 {
					t.Fatalf("lost safe current slots or included the bad slot: used=%d actions=%d horizon=%d", len(used), len(plan.Actions), plan.HorizonSlots)
				}
				if err := ValidatePlan(used, p, &plan); err != nil {
					t.Fatal(err)
				}
				if plan.Solver == nil || !plan.Solver.Fallback || !strings.Contains(plan.Solver.FallbackReason, "Plan ends at") {
					t.Fatalf("shorter plan hidden: %+v", plan.Solver)
				}
				if len(plan.LoadpointShortfallWh) != 0 {
					t.Fatal("a deadline beyond the plan was reported as missed")
				}
			})
		}
	}
}

func TestCoreRecoveryPreservesModeAndHandlesLongPartialHorizon(t *testing.T) {
	for _, mode := range []Mode{ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage, ModeArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			slots, p := recoveryFixture()
			p.Mode, p.InitialSoC = mode, p.SoCMin
			slots[12].LoadW = 19000
			plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidatePlan(used, p, &plan); err != nil {
				t.Fatal(err)
			}
			want := len(slots)
			if mode == ModeSelfConsumption {
				want = 12 // grid charging to prepare for the peak is forbidden
			}
			if len(used) != want || plan.Mode != mode {
				t.Fatalf("mode changed or wrong horizon: mode=%s slots=%d want=%d", plan.Mode, len(used), want)
			}
		})
	}
	slots, p := recoveryFixture()
	first := slots[0]
	for len(slots) < 193 {
		s := first
		s.StartMs += int64(len(slots)) * 15 * 60000
		slots = append(slots, s)
	}
	slots[0].ExecutionStartMs = slots[0].StartMs + 14*60000
	slots[100].LoadW = 40000
	plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
	if err != nil || len(used) != 100 {
		t.Fatalf("long partial horizon: slots=%d err=%v", len(used), err)
	}
	if plan.Actions[0].ExecutionStartMs != slots[0].ExecutionStartMs {
		t.Fatal("partial current interval was expanded")
	}
	if err := ValidatePlan(used, p, &plan); err != nil {
		t.Fatal(err)
	}
}

func TestCoreRecoveryRejectsUnsafeCurrentSlotAndInvalidInputs(t *testing.T) {
	for _, mutation := range []string{"current overload", "invalid timeline", "invalid limit", "canceled"} {
		t.Run(mutation, func(t *testing.T) {
			slots, p := recoveryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mutation {
			case "current overload":
				slots[0].LoadW = 40000
			case "invalid timeline":
				slots[0].LenMin = 0
			case "invalid limit":
				slots[0].Limits.MaxImportW = math.NaN()
			case "canceled":
				cancel()
			}
			plan, used, err := corePlanWithRecovery(ctx, slots, p)
			if err == nil || len(plan.Actions) != 0 || len(used) != 0 {
				t.Fatalf("unsafe or stale work became a plan: actions=%d slots=%d err=%v", len(plan.Actions), len(used), err)
			}
			if mutation == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryPublishesFullPriorAndReturnsToModel(t *testing.T) {
	for _, externalFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "core", true: "external failure"}[externalFailure], func(t *testing.T) {
			svc := shadowTestService(t)
			start := time.Now().UTC().Truncate(time.Hour)
			svc.now = func() time.Time { return start.Add(5 * time.Minute) }
			svc.Defaults.Mode = ModeArbitrage
			svc.FuseMaxW = 11040
			_, p := recoveryFixture()
			p.Loadpoint.TargetSlotIdx = 3
			svc.Loadpoint = func(int) *LoadpointSpec { return p.Loadpoint }
			badForecast := true
			var saved *Diagnostic
			var recorded int
			svc.SaveDiag = func(d *Diagnostic, _ string) error { saved = d; return nil }
			svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
				return ForecastInputs{Load: func(at time.Time) float64 {
					if badForecast && !at.Before(start.Add(2*time.Hour)) {
						return 40000
					}
					return 500
				}, Record: func(base, planning []Slot, _ string, _ int64) {
					if len(base) != len(planning) {
						t.Errorf("recorded mismatched forecasts: %d != %d", len(base), len(planning))
					}
					recorded = len(base)
				}}
			}
			if externalFailure {
				svc.Optimizer = failingPrimaryOptimizer{}
			}
			plan := svc.Replan(context.Background())
			snap := svc.PlanSnapshot()
			if plan == nil || len(plan.Actions) != 4 || snap.Outdated || snap.Error != "" || len(snap.Warnings) == 0 {
				t.Fatalf("full prior plan not published: %+v", snap)
			}
			if _, ok := svc.SlotDirectiveAt(svc.now()); !ok {
				t.Fatal("safe current plan cannot dispatch")
			}
			if _, ok := svc.SlotDirectiveAt(start.Add(2 * time.Hour)); !ok {
				t.Fatal("valid prior did not retain the full price horizon")
			}
			for _, slot := range svc.lastSlots {
				if slot.LoadW != 500 {
					t.Fatal("rejected forecast reached accepted plan")
				}
			}
			if saved == nil || len(saved.Slots) != 4 || saved.Horizon != 4 || recorded != 4 {
				t.Fatalf("diagnostic or forecast archive lost accepted horizon: saved=%+v recorded=%d", saved, recorded)
			}
			if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
				t.Fatal(err)
			}
			badForecast = false
			full := svc.Replan(context.Background())
			if full == nil || len(full.Actions) != 4 || len(svc.PlanSnapshot().Warnings) != 0 || svc.PlanSnapshot().Outdated {
				t.Fatalf("shortened horizon stuck after forecast recovered: %+v", svc.PlanSnapshot())
			}
			if saved.Horizon != 4 || len(saved.Slots) != 4 || recorded != 4 {
				t.Fatal("restored full horizon was not recorded")
			}
		})
	}
}

func TestNativeRecoveryForReportedFailureChain(t *testing.T) {
	worker := nativeWorker(t, time.Second)
	t.Cleanup(func() { _ = worker.Close() })
	slots, p := recoveryFixture()
	slots[12].LoadW = 40000
	if _, err := worker.Optimize(context.Background(), slots, p); err == nil || !strings.Contains(err.Error(), "infeasible") {
		t.Fatalf("fixture did not reproduce worker infeasibility: %v", err)
	}
	plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
	if err != nil || len(plan.Actions) != 12 {
		t.Fatalf("same worker failure still lost safe control: actions=%d err=%v", len(plan.Actions), err)
	}
	if err := ValidatePlan(used, p, &plan); err != nil {
		t.Fatal(err)
	}
}
