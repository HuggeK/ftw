package mpc

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

// A point load above the fuse can be feasible with battery support. Optional
// forecast caution must not turn that valid point forecast into a reserve plan.
func TestNativePlannerRecoversValidPointFromImpossibleLoadMargin(t *testing.T) {
	for _, backend := range []string{"native", "core", "worker unavailable"} {
		t.Run(backend, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				peak, power float64
				goalBlocked bool
			}{
				{"infeasible margin", 22400, 10000, false},
				{"feasible margin misses reachable departure", 12000, 0, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					svc := shadowTestService(t)
					var worker *ExternalOptimizer
					if backend == "native" {
						worker = nativeWorker(t, time.Second)
						svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
					} else if backend == "worker unavailable" {
						svc.Optimizer = &physicsGateFailingOptimizer{}
					}
					t.Cleanup(func() {
						svc.replanWG.Wait()
						svc.shadowWG.Wait()
						if worker != nil {
							_ = worker.Close()
						}
					})
					start := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
					origin := start.Add(5 * time.Minute)
					svc.now = func() time.Time { return origin }
					svc.FuseMaxW, svc.BaseLoad, svc.PVForecastSafetyK = 17250, 800, .6
					svc.Defaults.Mode = ModeArbitrage
					svc.Defaults.CapacityWh = 15000
					svc.Defaults.MaxChargeW, svc.Defaults.MaxDischargeW = tc.power, tc.power
					svc.Loadpoint = func(int) *LoadpointSpec {
						return &LoadpointSpec{ID: "car", CapacityWh: 86500, InitialSoC: .68,
							SoCMax: 1, Levels: 101, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 19,
							MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 6900, 10350},
							ChargeEfficiency: .9, NoBatteryToEV: true}
					}
					for i := 0; i < 20; i++ {
						price := 450.0
						if i >= 8 {
							price = 50
						}
						if err := svc.Store.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), SlotLenMin: 15,
							SpotOreKwh: price, TotalOreKwh: price, Source: "test", FetchedAtMs: start.UnixMilli()}}); err != nil {
							t.Fatal(err)
						}
					}
					calibrator := forecasting.NewCalibrator(nil, "test", origin.UnixMilli())
					var rejected []Slot
					svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
						return ForecastInputs{
							Load: func(at time.Time) float64 {
								if tc.goalBlocked || !at.Before(start.Add(4*time.Hour)) {
									return tc.peak
								}
								return 1800
							},
							Risk: func(base, planning []Slot, k float64) {
								for i, s := range base {
									band := calibrator.BandForInterval("legacy_shadow", "load", s.StartMs, s.StartMs+int64(s.LenMin)*60000, s.LoadW)
									planning[i].LoadW = s.LoadW + k*(band.HighW-s.LoadW)
								}
								rejected = append([]Slot(nil), planning...)
							},
						}
					}
					plan := svc.Replan(context.Background())
					svc.shadowWG.Wait()
					if plan == nil || plan.Solver == nil || len(plan.Actions) != 20 || (backend == "native" && (plan.Solver.Engine != "native" || plan.Solver.Fallback)) {
						t.Fatalf("optional caution disabled native planning: plan=%+v status=%+v", plan, svc.PlanSnapshot())
					}
					if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
						t.Fatal(err)
					}
					if (!tc.goalBlocked && plan.Actions[0].LoadpointW > 1e-6) || math.Abs(plan.Actions[19].LoadpointSoC-.8) > 1e-5 {
						t.Fatalf("optional margin lost the reachable departure goal: solver=%+v shortfall=%v", plan.Solver, plan.LoadpointShortfallWh)
					}
					if len(rejected) != 20 || math.Abs(rejected[16].LoadW-tc.peak*1.27) > 1e-6 || svc.lastSlots[16].LoadW != tc.peak {
						t.Fatal("recovery changed a feasible point peak or used the configured prior")
					}
					if !strings.Contains(strings.Join(svc.PlanSnapshot().Warnings, " "), "point forecast") {
						t.Fatal("dropping the impossible risk margin was hidden")
					}
					if worker == nil {
						return
					}
					// Isolate the house constraint from the car. One margin makes even
					// the house impossible; the other fits the house but blocks the car.
					p := svc.lastParams
					p.Loadpoint, p.Loadpoints = nil, nil
					withoutCar, err := worker.Optimize(context.Background(), rejected, p)
					if tc.goalBlocked {
						if err != nil {
							t.Fatalf("house-only risk plan: %v", err)
						}
						if err := ValidatePlan(rejected, p, &withoutCar); err != nil {
							t.Fatal(err)
						}
					} else if !forecastInfeasible(err) {
						t.Fatalf("impossible house margin was attributed to the car: %v", err)
					}
				})
			}
		})
	}
}
