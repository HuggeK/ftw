package loadpoint

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func TestFuseDemandsPreserveIntentBehindZeroCap(t *testing.T) {
	for _, mode := range []string{"scheduled", "manual", "manual_1p", "manual_current_limit", "fallback", "price_pause", "manual_stop", "no_surplus", "vehicle_complete", "fallback_power_unknown"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			cfg := fuseRecoveryLoadpoint()
			cfg.VehicleCapacityWh, cfg.PluginSoC = 60000, .3
			cfg.AllowedStepsW = append(cfg.AllowedStepsW, 1380, 2300, 3680)
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{cfg.ID: 2587.5}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 10350}}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			wantW, phase := 10350.0, "3p"
			switch mode {
			case "price_pause":
				d.LoadpointEnergyWh[cfg.ID], wantW = 0, 0
			case "manual_stop", "no_surplus", "vehicle_complete", "fallback_power_unknown":
				wantW = 0
			}
			if mode == "no_surplus" {
				cfg.SurplusOnly = true
			}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, d, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetFuseEVMax(func() (float64, bool) { return 0, true })
			switch mode {
			case "manual":
				c.SetManualHold(cfg.ID, ManualHold{PowerW: wantW, PhaseMode: phase, Persistent: true})
			case "manual_1p":
				wantW, phase = 3680, "1p"
				c.SetManualHold(cfg.ID, ManualHold{PowerW: wantW, PhaseMode: phase, Persistent: true})
			case "manual_current_limit":
				wantW = 4140
				c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: phase, MaxAmpsPerPhase: 6, Persistent: true})
			case "manual_stop":
				c.SetManualHold(cfg.ID, ManualHold{Persistent: true})
			case "no_surplus":
				c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })
			case "vehicle_complete":
				c.manager.SetSchedule(cfg.ID, Schedule{FinishAtVehicleLimit: true, TimeOfDayMinUTC: 13 * 60, Recurring: true})
				c.manager.RollSchedules(now)
				c.SetVehicleChargeState(func(string) (VehicleChargeState, bool) {
					return VehicleChargeState{SoC: .8, Limit: .8, State: "Complete"}, true
				})
			case "fallback", "fallback_power_unknown":
				d.LoadpointFallback = map[string]bool{cfg.ID: true}
				c.manager.SetTarget(cfg.ID, .8, now.Add(time.Hour))
				if mode == "fallback_power_unknown" {
					samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerUnavailable: true}
				}
			}
			c.Tick(context.Background(), now)
			set, ok := lastSetCurrent(sender.calls)
			if !ok || set.power != 0 {
				t.Fatalf("zero physical cap did not stop: %+v", set)
			}
			got := c.FuseDemands()
			if wantW == 0 {
				if len(got) != 0 {
					t.Fatalf("inactive intent reserved fuse room: %+v", got)
				}
			} else {
				if len(got) != 1 || got[0].DriverName != cfg.DriverName || got[0].PowerW != wantW || got[0].PhaseMode != phase {
					t.Fatalf("pre-clamp intent lost: got=%+v want=%.0f W/%s", got, wantW, phase)
				}
				got[0].PowerW = 1
				if c.FuseDemands()[0].PowerW != wantW {
					t.Fatal("snapshot aliases the controller")
				}
			}
			c.TickWithDispatch(context.Background(), now.Add(5*time.Second), false)
			if got := c.FuseDemands(); len(got) != 0 {
				t.Fatalf("stale meter retained demand: %+v", got)
			}
		})
	}
}

func TestFuseDemandsDeduplicateDriverAndCopySnapshots(t *testing.T) {
	c := &Controller{}
	c.recordFuseDemand("charger", 4000, "3p")
	c.recordFuseDemand("charger", 5000, "1p")
	c.recordFuseDemand("charger", 2000, "3p")
	c.recordFuseDemand("other", math.NaN(), "3p")
	c.recordFuseDemand("other", math.Inf(1), "3p")
	c.recordFuseDemand("other", 0, "3p")
	got := c.FuseDemands()
	if len(got) != 1 || got[0].PowerW != 5000 || got[0].PhaseMode != "auto" {
		t.Fatalf("duplicate driver must use max power and unknown phase: %+v", got)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c.recordFuseDemand("charger", 5000, "3p")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			copy := c.FuseDemands()
			copy[0].PowerW = -1
		}
	}()
	wg.Wait()
	if c.FuseDemands()[0].PowerW != 5000 {
		t.Fatal("concurrent snapshot changed controller state")
	}
}
