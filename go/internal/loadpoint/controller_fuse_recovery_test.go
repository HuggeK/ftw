package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

func fuseRecoveryLoadpoint() Config {
	cfg := reasonLoadpoint()
	cfg.MaxChargeW = 11040
	cfg.AllowedStepsW = []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}
	return cfg
}

func TestFuseBudgetRecoversAfterTwoSafeTicks(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, initial := range []struct {
			name string
			capW float64
		}{{"below_minimum", 3560}, {"zero", 0}, {"invalid", math.NaN()}} {
			name := "scheduled/" + initial.name
			if manual {
				name = "manual/" + initial.name
			}
			t.Run(name, func(t *testing.T) {
				now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
				cfg := fuseRecoveryLoadpoint()
				d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{cfg.ID: 2587.5}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 10350}}
				samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true, PowerW: 10000}}
				sender := &fakeSender{}
				c := newTestController(t, []Config{cfg}, d, samples, sender)
				c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
				if manual {
					c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
				}
				capW := initial.capW
				c.SetFuseEVMax(func() (float64, bool) { return capW, true })
				check := func(at time.Time, allowed bool, want float64) {
					t.Helper()
					c.TickWithDispatch(context.Background(), at, allowed)
					set, ok := lastSetCurrent(sender.calls)
					if !ok || set.power != want {
						t.Fatalf("at %v: command=%+v, want %.0f W", at.Sub(now), set, want)
					}
				}
				check(now, true, 0)
				capW = 13560
				check(now.Add(5*time.Second), true, 0)
				check(now.Add(10*time.Second), true, 10350)
				// A later loss of headroom must stop immediately and require
				// two safe dispatch ticks again, never a five-minute lock.
				capW = 3000
				check(now.Add(15*time.Second), true, 0)
				capW = 13560
				check(now.Add(20*time.Second), true, 0)
				check(now.Add(25*time.Second), false, 0)
				check(now.Add(30*time.Second), true, 0)
				check(now.Add(35*time.Second), true, 10350)
				// A saved Stop remains a Stop after the fuse budget recovers.
				c.SetManualHold(cfg.ID, ManualHold{Persistent: true})
				check(now.Add(40*time.Second), true, 0)
			})
		}
	}
}

func TestFirstConnectionIsObservedBeforeFuseBudget(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true, PowerW: 10000}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	seenConnected := false
	c.SetFuseEVMax(func() (float64, bool) {
		st, _ := c.manager.State(cfg.ID)
		seenConnected = st.PluggedIn
		if !st.PluggedIn {
			return 3560, true
		}
		return 13560, true
	})
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !seenConnected || !ok || set.power != 10350 {
		t.Fatalf("fresh connection absent from shared budget: connected=%v command=%+v", seenConnected, set)
	}
}

func TestSharedPhaseBudgetDoesNotIntegrateSameOverload(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true, PowerW: 8280}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	// Our own 12 A offer is established before the overload; a reading
	// from the car is never what the guard credits.
	worstA, otherA, budgetW := 12.0, 12.0, 8280.0
	c.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) { return worstA, otherA, otherA, true })
	c.SetFuseEVMax(func() (float64, bool) { return budgetW, true })
	c.Tick(context.Background(), now.Add(-5*time.Second))
	worstA, otherA, budgetW = 27, 20, 6210
	for tick := 0; tick < 12; tick++ {
		c.Tick(context.Background(), now.Add(time.Duration(tick)*5*time.Second))
		set, ok := lastSetCurrent(sender.calls)
		// Offered EV=12 A, worst site phase=27 A. A 9 A EV offer
		// leaves that phase at 24 A, below the 25 A fuse. Re-reading
		// the same sample cannot justify a second accumulated trim.
		if !ok || set.power != 6210 {
			t.Fatalf("tick %d: command=%+v, want stable legal 6210 W", tick, set)
		}
	}
}

func TestManualSinglePhaseUsesItsLegalMinimumInSharedFuseBudget(t *testing.T) {
	for _, phaseMode := range []string{"1p", "auto"} {
		t.Run(phaseMode, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			cfg := fuseRecoveryLoadpoint()
			cfg.AllowedStepsW = append(cfg.AllowedStepsW, 1380, 2300, 3680)
			sender := &fakeSender{}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			c := newTestController(t, []Config{cfg}, nil, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetManualHold(cfg.ID, ManualHold{PowerW: 3680, PhaseMode: phaseMode, Persistent: true})
			c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
			c.Tick(context.Background(), now)
			set, ok := lastSetCurrent(sender.calls)
			if !ok || set.power != 3680 {
				t.Fatalf("legal single-phase offer was treated as below the three-phase minimum: %+v", set)
			}
		})
	}
}

func TestNewConnectionDoesNotInheritFuseRecovery(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	capW := 0.0
	c.SetFuseEVMax(func() (float64, bool) { return capW, true })
	c.Tick(context.Background(), now)
	capW = 13560
	c.Tick(context.Background(), now.Add(5*time.Second))
	samples[cfg.DriverName] = EVSample{Connected: false}
	c.Tick(context.Background(), now.Add(10*time.Second))
	samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true}
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now.Add(15*time.Second))
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 10350 {
		t.Fatalf("new session inherited the previous session's recovery wait: %+v", set)
	}
}

func TestManualPhaseOverrideKeepsSixAmpMinimum(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	cfg.MinChargeW, cfg.AllowedStepsW = 1380, nil
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 1000, PhaseMode: "1p", Persistent: true})
	c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 0 || len(c.FuseDemands()) != 0 {
		t.Fatalf("sub6A request became a charger or battery reservation: command=%+v demand=%+v", set, c.FuseDemands())
	}
}

func TestUnspecifiedSurplusPhaseKeepsSinglePhaseFuseBound(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1380, MaxChargeW: 11040, SurplusOnly: true,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11040}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) { return 20, 20, 20, true })
	// A hold without a phase override inherits surplus mode's auto choice.
	// At2760 W that may be a12 A offer on the already20 A phase.
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 2760, Persistent: true})
	c.SetFuseEVMax(func() (float64, bool) { return 2760, true })
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 0 {
		t.Fatalf("only4 A remains per phase, no legal6 A auto offer fits: %+v", set)
	}
}

func TestUnknownFuseVerdictIsFrozenForAllCars(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	configs := []Config{fuseRecoveryLoadpoint(), fuseRecoveryLoadpoint()}
	configs[0].ID, configs[0].DriverName = "a", "charger-a"
	configs[1].ID, configs[1].DriverName = "b", "charger-b"
	sender := &fakeSender{}
	samples := map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}
	c := newTestController(t, configs, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	for _, cfg := range configs {
		c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	}
	reads := 0
	c.SetFuseEVMax(func() (float64, bool) {
		reads++
		return 5000, reads > 1
	})
	c.Tick(context.Background(), now)
	if reads != 1 {
		t.Fatalf("one unknown verdict became different per-car budgets: reads=%d", reads)
	}
	// (_, false) keeps its existing no-constraint meaning for the whole
	// first tick. Only the next tick may adopt the new shared5000 W cap.
	var firstTotal float64
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			firstTotal += cmd.power
		}
	}
	if firstTotal != 8280 {
		t.Fatalf("unknown verdict changed its no-constraint meaning: %.0f W", firstTotal)
	}
	sender.calls = nil
	c.Tick(context.Background(), now.Add(5*time.Second))
	var nextTotal float64
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			nextTotal += cmd.power
		}
	}
	if reads != 2 || nextTotal <= 0 || nextTotal > 5000 {
		t.Fatalf("next known budget was not shared: reads=%d total=%.0f W", reads, nextTotal)
	}
}

func TestManualCurrentCeilingLeavesFuseRoomForSecondCar(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	configs := []Config{fuseRecoveryLoadpoint(), fuseRecoveryLoadpoint()}
	configs[0].ID, configs[0].DriverName = "a", "charger-a"
	configs[1].ID, configs[1].DriverName = "b", "charger-b"
	sender := &fakeSender{}
	samples := map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}
	c := newTestController(t, configs, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetFuseEVMax(func() (float64, bool) { return 9000, true })
	c.SetManualHold("a", ManualHold{PowerW: 10350, PhaseMode: "3p", MaxAmpsPerPhase: 6, Persistent: true})
	c.SetManualHold("b", ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now)
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
		}
	}
	if commands["charger-a"] != 4140 || commands["charger-b"] != 4140 {
		t.Fatalf("current-limited car reserved unused fuse room: %+v", commands)
	}
}
