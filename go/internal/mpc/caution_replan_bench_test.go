package mpc

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Replan wall time to a published plan with forecast caution on (Balanced,
// safety_k 0.3) and off (0), on the production planners and real horizons.
// The Energyplan cases need the bundled worker. From go/:
//
//	FTW_NATIVE_SOLVER=$(python3 ../optimizer/native/verify.py --host-binary) \
//	  go test ./internal/mpc -run '^$' -bench NativeCautionReplan -benchtime 5x
func BenchmarkNativeCautionReplan(b *testing.B) {
	// A morning and an evening plug-in, with and without battery_covers_ev.
	// The caution-off metric counts plans that fell back to the point plan.
	starts := map[string]time.Time{
		"morning": time.Date(2026, 10, 16, 7, 45, 0, 0, time.UTC),
		"evening": time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC),
	}
	for _, planner := range []string{"energyplan", "core"} {
		for _, at := range []string{"morning", "evening"} {
			for _, site := range []struct {
				name     string
				slots    int
				ev       bool
				coversEV bool
			}{
				{"battery/35h", 140, false, false},
				{"battery/48h", 193, false, false},
				{"battery+ev/35h", 140, true, false},
				{"battery+ev/48h", 193, true, false},
				{"battery-covers-ev/35h", 140, true, true},
				{"battery-covers-ev/48h", 193, true, true},
			} {
				for _, k := range []float64{0, .3} {
					b.Run(fmt.Sprintf("%s/%s/%s/k=%g", planner, at, site.name, k), func(b *testing.B) {
						svc := cautionBenchService(b, starts[at], site.slots, site.ev, site.coversEV, k)
						if planner == "energyplan" {
							template := nativeWorker(b, time.Second)
							o, err := NewEnergyplanOptimizer(template.cfg.Command[0])
							_ = template.Close()
							if err != nil {
								b.Fatal(err)
							}
							b.Cleanup(func() { _ = o.Close() })
							svc.Optimizer = o
							// Start the worker outside the timed replans.
							if plan := svc.Replan(context.Background()); plan == nil {
								b.Fatalf("no warm-up plan: %+v", svc.PlanSnapshot())
							}
							svc.shadowWG.Wait()
						}
						var walls []float64
						off := 0
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							start := time.Now()
							plan := svc.Replan(context.Background())
							wall := time.Since(start)
							b.StopTimer()
							if plan == nil {
								b.Fatalf("no plan: %+v", svc.PlanSnapshot())
							}
							if plan.CautionOff {
								if off++; off == 1 {
									b.Logf("caution off: %q", svc.PlanSnapshot().Warnings)
								}
							}
							walls = append(walls, float64(wall.Microseconds())/1000)
							svc.shadowWG.Wait()
							b.StartTimer()
						}
						slices.Sort(walls)
						b.ReportMetric(walls[len(walls)/2], "median-ms")
						b.ReportMetric(walls[len(walls)-1], "max-ms")
						b.ReportMetric(float64(off), "caution-off")
					})
				}
			}
		}
	}
}

func cautionBenchService(b *testing.B, start time.Time, slots int, ev, coversEV bool, k float64) *Service {
	b.Helper()
	st, err := state.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	prices := make([]state.PricePoint, slots)
	for i := range prices {
		at := start.Add(time.Duration(i) * 15 * time.Minute)
		hour := float64(at.Hour()) + float64(at.Minute())/60
		// A two-peak day: cheap nights and middays, dear mornings and evenings.
		spot := 60 + 70*math.Max(0, math.Sin((hour-5)/24*4*math.Pi)) + 5*math.Sin(float64(i))
		prices[i] = state.PricePoint{Zone: "SE3", SlotTsMs: at.UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: spot, TotalOreKwh: 1.25*spot + 80, Source: "bench", FetchedAtMs: start.UnixMilli()}
	}
	if err := st.SavePrices(prices); err != nil {
		b.Fatal(err)
	}
	svc := New(st, nil, "SE3", Params{Mode: ModeArbitrage, SoCLevels: 51, ActionLevels: 21,
		CapacityWh: 10000, InitialSoC: .5, SoCMin: .1, SoCMax: .95, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.now = func() time.Time { return start.Add(time.Minute) }
	svc.FuseMaxW, svc.BaseLoad, svc.PVForecastSafetyK = 17250, 800, k
	if ev {
		svc.Loadpoint = func(int) *LoadpointSpec {
			return &LoadpointSpec{ID: "car", CapacityWh: 75000, InitialSoC: .4, SoCMax: 1, PluggedIn: true,
				TargetSoC: .8, TargetSlotIdx: 28, Levels: 101, MaxChargeW: 11000, MinChargeW: 4140,
				AllowedStepsW: []float64{0, 4140, 6900, 11000}, ChargeEfficiency: .9, NoBatteryToEV: !coversEV}
		}
	}
	solar := func(at time.Time) float64 {
		hour := float64(at.Hour()) + float64(at.Minute())/60
		return 6000 * math.Max(0, math.Sin((hour-6)/12*math.Pi))
	}
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		return ForecastInputs{
			PVRelativeUncertainty: .25,
			Resolve: func(_ context.Context, in []Slot) []Slot {
				for i := range in {
					at := time.UnixMilli(in[i].StartMs)
					in[i].PVW = -solar(at)
					in[i].LoadW = 700 + 500*math.Max(0, math.Sin(float64(at.Hour()-16)/8*math.Pi))
				}
				return in
			},
			// The host's calibrated margin: PV downside and house-load upside.
			Risk: func(base, planning []Slot, k float64) {
				for i := range planning {
					planning[i].PVW = base[i].PVW * (1 - .5*k)
					planning[i].LoadW = base[i].LoadW + k*400
				}
			},
		}
	}
	return svc
}
