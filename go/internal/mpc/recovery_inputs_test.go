package mpc

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestRecoveryPublishesBeforeCorruptFutureInputs(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, fault := range []string{"nan pv", "infinite price"} {
			t.Run(fault+map[bool]string{false: "/core", true: "/worker failure"}[external], func(t *testing.T) {
				svc := shadowTestService(t)
				start := time.Now().UTC().Truncate(time.Hour)
				svc.now = func() time.Time { return start.Add(5 * time.Minute) }
				svc.Defaults.TerminalSoCPrice = 0 // derive it without the broken future tariff
				if external {
					svc.Optimizer = failingPrimaryOptimizer{}
				}
				bad := true
				var saved *Diagnostic
				recorded := 0
				svc.SaveDiag = func(d *Diagnostic, _ string) error { saved = d; return nil }
				svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
					return ForecastInputs{
						Resolve: func(_ context.Context, slots []Slot) []Slot {
							if fault == "nan pv" && bad {
								slots[2].PVW = math.NaN()
							}
							return slots
						},
						Record: func(base, planning []Slot, _ string, _ int64) {
							if len(base) != len(planning) {
								t.Errorf("recorded horizons differ: %d != %d", len(base), len(planning))
							}
							recorded = len(planning)
						},
					}
				}
				setPrice := func(price float64) {
					t.Helper()
					if err := svc.Store.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: start.Add(2 * time.Hour).UnixMilli(),
						SlotLenMin: 60, SpotOreKwh: price, TotalOreKwh: price, Source: "test", FetchedAtMs: start.UnixMilli()}}); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "infinite price" {
					setPrice(math.Inf(1))
				}
				plan := svc.Replan(context.Background())
				if plan == nil || len(plan.Actions) != 2 || len(svc.PlanSnapshot().Warnings) == 0 || svc.PlanSnapshot().Outdated {
					t.Fatalf("corrupt future input lost safe plan: %+v", svc.PlanSnapshot())
				}
				assertRecoveryPlan(t, svc.lastSlots, svc.lastParams, *plan)
				if !strings.Contains(plan.Solver.FallbackReason, "invalid") || saved == nil || len(saved.Slots) != 2 || recorded != 2 {
					t.Fatalf("missing failure reason or accepted inputs: solver=%+v saved=%v recorded=%d", plan.Solver, saved != nil, recorded)
				}
				if _, ok := svc.SlotDirectiveAt(start.Add(2 * time.Hour)); ok {
					t.Fatal("dispatch crossed the damaged interval")
				}
				bad = false
				setPrice(200)
				restored := svc.Replan(context.Background())
				if restored == nil || len(restored.Actions) != 4 || recorded != 4 || len(svc.PlanSnapshot().Warnings) != 0 {
					t.Fatalf("did not recover full horizon: %+v", svc.PlanSnapshot())
				}
			})
		}
	}
}

func TestRecoveryMissingBatteryStateCannotInventEnergy(t *testing.T) {
	for _, fleet := range []bool{false, true} {
		for _, fault := range []string{"missing reading", "missing soc", "aged soc", "nan soc", "offline", "command fault"} {
			t.Run(fault+map[bool]string{false: "/aggregate", true: "/fleet"}[fleet], func(t *testing.T) {
				svc := shadowTestService(t)
				svc.Tele = telemetry.NewStore()
				if fleet {
					svc.BatteryFleet = []BatteryFleetMember{{Driver: "battery", CapacityWh: 10000, MaxChargeW: 2000, MaxDischargeW: 2000}}
				}
				soc := .5
				svc.Tele.Update("battery", telemetry.DerBattery, 0, &soc, nil)
				svc.Tele.RecordDriverSuccess("battery")
				accepted := svc.Replan(context.Background())
				if accepted == nil {
					t.Fatal("no healthy baseline")
				}
				switch fault {
				case "missing reading":
					svc.Tele = telemetry.NewStore()
				case "missing soc":
					svc.Tele.Get("battery", telemetry.DerBattery).SoC = nil
				case "nan soc":
					bad := math.NaN()
					svc.Tele.Get("battery", telemetry.DerBattery).SoC = &bad
				case "aged soc":
					// A different reading may keep driver health live after its
					// battery SoC source stops. No goroutine shares this fixture.
					svc.Tele.Get("battery", telemetry.DerBattery).SoCUpdatedAt = time.Now().Add(-telemetry.BatterySoCMaxAge - time.Second)
				case "offline":
					svc.Tele.DriverHealthMut("battery").SetOffline()
				case "command fault":
					svc.Tele.SetDriverCommandFault("battery", true, "command refused")
				}
				got := svc.Replan(context.Background())
				if got != accepted || !svc.PlanSnapshot().Outdated || svc.PlanSnapshot().Error == "" {
					t.Fatalf("unavailable battery became new energy: %+v", svc.PlanSnapshot())
				}
				if _, ok := svc.SlotDirectiveAt(time.Now()); ok {
					t.Fatal("unavailable battery still has an executable directive")
				}
				svc.Tele.Update("battery", telemetry.DerBattery, 0, &soc, nil)
				svc.Tele.RecordDriverSuccess("battery")
				svc.Tele.SetDriverCommandFault("battery", false, "")
				if svc.Replan(context.Background()) == nil || svc.PlanSnapshot().Outdated {
					t.Fatalf("fresh state did not restore planning: %+v", svc.PlanSnapshot())
				}
			})
		}
	}
}
