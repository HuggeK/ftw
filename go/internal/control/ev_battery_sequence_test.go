package control

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestJoinedEVWaitsForMeasuredBatteryBackoffThenResumes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	const houseW, desiredEVW = 5000.0, 10350.0
	actualBattery, actualEV := 8000.0, 0.0
	config := loadpoint.Config{ID: "garage", DriverName: "charger", MinChargeW: 4140, MaxChargeW: desiredEVW,
		PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{config})
	store := telemetry.NewStore()
	dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 2500, Strategy: "arbitrage"}
	st := newStateWithEnergyDispatch(dir, "meter")
	st.clock = func() time.Time { return now }
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
	st.SlewEnabled = false
	st.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	var sampledPhaseA float64
	controller := loadpoint.NewController(mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: actualEV, PowerAt: now, SessionWhUnavailable: true}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				actualEV = cmd.PowerW
				// The physical battery may ignore a reduction. Check this at
				// the EV send, before a later battery command can conceal it.
				if phase := (houseW + actualBattery + actualEV) / 690; phase > 24.001 {
					t.Fatalf("EV spent unmeasured battery relief: %.1f A", phase)
				}
			}
			return nil
		})
	controller.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	controller.SetManualHold(config.ID, loadpoint.ManualHold{PowerW: desiredEVW, PhaseMode: "3p", Persistent: true})
	controller.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) { return sampledPhaseA, sampledPhaseA, sampledPhaseA, true })
	controller.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(store, st, mgr.States(), mgr.Configs(), 17250) })
	emit := func(driver string, kind telemetry.DerType, watts float64) {
		d := map[string]any{"control_power_available": true, "control_power_observed_at": now.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			d["l1_a"], d["l2_a"], d["l3_a"] = sampledPhaseA, sampledPhaseA, sampledPhaseA
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.Get(driver, kind).UpdatedAt = now
		store.RecordDriverSuccess(driver)
	}
	var obeyedAt, resumedAt time.Time
	for tick := 0; tick < 30; tick++ {
		now = now.Add(5 * time.Second)
		if tick == 3 {
			actualBattery, obeyedAt = 0, now // Only now does the battery obey.
		}
		sampledPhaseA = (houseW + actualBattery + actualEV) / 690
		emit("pixii", telemetry.DerBattery, actualBattery)
		emit(config.DriverName, telemetry.DerEV, actualEV)
		emit("meter", telemetry.DerMeter, houseW+actualBattery+actualEV)
		controller.TickWithDispatch(context.Background(), now, true)
		st.EVCommandedW = loadpoint.CommandedEVPowerW(mgr.States())
		st.SetEVFuseDemands(controller.FuseDemands())
		targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15000}), 17250)
		if len(targets) != 1 || targets[0].TargetW > 1210.001 {
			t.Fatalf("tick %d: battery did not yield to the EV: %+v", tick, targets)
		}
		if actualEV > 0 && resumedAt.IsZero() {
			resumedAt = now
		}
		if !obeyedAt.IsZero() {
			actualBattery = math.Max(0, targets[0].TargetW)
		}
		if (houseW+actualBattery+actualEV)/690 > 24.001 {
			t.Fatalf("tick %d: battery took the EV's headroom: EV %.0f battery %.0f W", tick, actualEV, actualBattery)
		}
	}
	if resumedAt.IsZero() || resumedAt.Sub(obeyedAt) < evRestartDwell {
		t.Fatalf("EV resumed at %v, battery obeyed at %v: want a %v dwell on measured room", resumedAt, obeyedAt, evRestartDwell)
	}
	if actualEV < 9660 {
		t.Fatalf("EV stuck at %.0f W after recovery", actualEV)
	}
}

// Whatever the EV guard may still allow, our battery orders keep the total
// inside 24 A, in either order of arrival.
func TestEVBatteryCommandOrdersStayWithinMeasuredHeadroom(t *testing.T) {
	rng := rand.New(rand.NewSource(1667))
	for sample := 0; sample < 300; sample++ {
		house := rng.Float64() * 5000
		first, second := rng.Float64()*4000, rng.Float64()*4000
		ev := rng.Float64() * 3000
		grid := house + first + second + ev
		requestedEV := rng.Float64() * 11040
		actualEV := math.Max(ev, math.Min(requestedEV, ev+16560-grid))
		amps := grid / 690
		store, state := evSplitFixture(t, grid, []float64{amps, amps, amps}, requestedEV, "3p")
		sent(state, "ev", ev)
		sent(state, "first", first)
		sent(state, "second", second)
		out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: rng.Float64() * 10000}, {Driver: "second", TargetW: rng.Float64() * 10000}}, store, state, 17250)
		for _, order := range [][2]int{{0, 1}, {1, 0}} {
			batteries := [2]float64{first, second}
			for _, i := range order {
				// A battery may ignore a reduction while its sibling obeys.
				batteries[i] = math.Max(batteries[i], out[i].TargetW)
				if total := house + actualEV + batteries[0] + batteries[1]; total > 16560.001 {
					t.Fatalf("sample %d order %v: grid %.1f W exceeds 24 A; before %.1f W targets %+v", sample, order, total, grid, out)
				}
			}
		}
	}
}

// Battery orders the meter may not show yet, the EV cap and new battery
// orders share one physical budget.
func TestEVBatteryPendingAndNewOrdersShareOnePhysicalBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(1668))
	for sample := 0; sample < 300; sample++ {
		house, first, second, ev := rng.Float64()*3000, rng.Float64()*2000, rng.Float64()*2000, rng.Float64()*2000
		grid := house + first + second + ev
		pendingFirst := first + rng.Float64()*(16560-grid)
		pendingSecond := second + rng.Float64()*(16560-grid-(pendingFirst-first))
		requestedEV := rng.Float64() * 11040
		amps := grid / 690
		store, state := evSplitFixture(t, grid, []float64{amps, amps, amps}, requestedEV, "3p")
		sent(state, "ev", ev)
		sent(state, "first", first, pendingFirst)
		sent(state, "second", second, pendingSecond)
		points := []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true, CommandedKnown: true, CommandedW: ev}}
		capW, _ := EVFuseBudget(store, state, points, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
		actualEV := math.Max(ev, math.Min(requestedEV, capW))
		out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: rng.Float64() * 10000}, {Driver: "second", TargetW: rng.Float64() * 10000}}, store, state, 17250)
		worst := house + actualEV + math.Max(pendingFirst, out[0].TargetW) + math.Max(pendingSecond, out[1].TargetW)
		if worst > 16560.001 {
			t.Fatalf("sample %d: old/new battery orders and EV reach %.1f W; pending %.1f/%.1f EV %.1f targets %+v", sample, worst, pendingFirst, pendingSecond, actualEV, out)
		}
	}
}

// evBatterySite runs the EV guard and battery dispatch together, as main.go
// does: charger tick first, then battery dispatch, then the next reading.
type evBatterySite struct {
	t                *testing.T
	now              time.Time
	store            *telemetry.Store
	state            *State
	mgr              *loadpoint.Manager
	ctrl             *loadpoint.Controller
	houseW, batteryW float64
	evW              float64
	commands, offers []evTimed
	maxGridW         float64
}

func newEVBatterySite(t *testing.T, houseW float64) *evBatterySite {
	t.Helper()
	s := &evBatterySite{t: t, now: time.Now().Truncate(time.Second), store: telemetry.NewStore(), houseW: houseW}
	config := loadpoint.Config{ID: "garage", DriverName: "charger", MinChargeW: 4140, MaxChargeW: 10350,
		PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	s.mgr = loadpoint.NewManager()
	s.mgr.Load([]loadpoint.Config{config})
	dir := SlotDirective{SlotStart: s.now, SlotEnd: s.now.Add(time.Hour), BatteryEnergyWh: 10000, Strategy: "arbitrage"}
	s.state = newStateWithEnergyDispatch(dir, "meter")
	s.state.clock = func() time.Time { return s.now }
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases, s.state.SiteFuseSafetyA = 25, 230, 3, 1
	s.state.SlewEnabled = false
	s.state.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	s.ctrl = loadpoint.NewController(s.mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: s.evW, PowerAt: s.now, SessionWhUnavailable: true}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				s.offers = append(s.offers, evTimed{s.now, cmd.PowerW})
				s.commands = append(s.commands, evTimed{s.now.Add(5 * time.Second), cmd.PowerW})
			}
			return nil
		})
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetManualHold(config.ID, loadpoint.ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	s.ctrl.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) {
		a := s.gridW() / 690
		return a, a, a, true
	})
	s.ctrl.SetFuseEVMax(func() (float64, bool) { return EVFuseBudget(s.store, s.state, s.mgr.States(), s.mgr.Configs(), 17250) })
	return s
}

func (s *evBatterySite) gridW() float64 { return s.houseW + s.batteryW + s.evW }

func (s *evBatterySite) emit(driver string, kind telemetry.DerType, watts float64) {
	d := map[string]any{"control_power_available": true, "control_power_observed_at": s.now.Format(time.RFC3339Nano)}
	if kind == telemetry.DerMeter {
		a := watts / 690
		d["l1_a"], d["l2_a"], d["l3_a"] = a, a, a
	}
	data, _ := json.Marshal(d)
	soc := .5
	s.store.Update(driver, kind, watts, &soc, data)
	s.store.Get(driver, kind).UpdatedAt = s.now
	s.store.RecordDriverSuccess(driver)
}

func (s *evBatterySite) step() {
	s.now = s.now.Add(time.Second)
	for len(s.commands) > 0 && !s.commands[0].at.After(s.now) {
		s.evW, s.commands = s.commands[0].w, s.commands[1:]
	}
	s.maxGridW = math.Max(s.maxGridW, s.gridW())
	s.emit("pixii", telemetry.DerBattery, s.batteryW)
	s.emit("charger", telemetry.DerEV, s.evW)
	s.emit("meter", telemetry.DerMeter, s.gridW())
	s.ctrl.TickWithDispatch(context.Background(), s.now, true)
	s.state.EVCommandedW = loadpoint.CommandedEVPowerW(s.mgr.States())
	s.state.SetEVFuseDemands(s.ctrl.FuseDemands())
	targets := ComputeDispatch(s.store, s.state, caps(map[string]float64{"pixii": 15000}), 17250)
	if len(targets) != 1 {
		s.t.Fatalf("targets %+v", targets)
	}
	s.batteryW = targets[0].TargetW // A local battery follows within a tick.
}

func (s *evBatterySite) run(d time.Duration) {
	for end := s.now.Add(d); s.now.Before(end); {
		s.step()
	}
}

func (s *evBatterySite) offer() float64 {
	if len(s.offers) == 0 {
		return 0
	}
	return s.offers[len(s.offers)-1].w
}

// A 2 kW house step lands while the battery and the car both charge. EV
// first: the car must not stop and the battery must yield. The battery
// answers within a tick, inside the fuse's heat budget, so the car keeps its
// offer; a car that is cut must get back to it within a minute.
func TestEVFirstHouseStepWhileBatteryAndEVCharge(t *testing.T) {
	s := newEVBatterySite(t, 3000)
	s.run(3*time.Minute + 30*time.Second)
	low, high := math.Inf(1), math.Inf(-1)
	for range 30 {
		s.step()
		low, high = math.Min(low, s.batteryW), math.Max(high, s.batteryW)
	}
	if high-low > 1 {
		t.Fatalf("battery hunts under a steady load: %.0f..%.0f W", low, high)
	}
	evBefore, batteryBefore := s.offer(), s.batteryW
	if evBefore != 10350 || batteryBefore <= 0 {
		t.Fatalf("setup: EV %.0f W battery %.0f W", evBefore, batteryBefore)
	}
	stepAt := s.now
	s.houseW += 2000
	var lowest float64 = evBefore
	var backAt time.Time
	for s.now.Sub(stepAt) < 3*time.Minute {
		s.step()
		lowest = math.Min(lowest, s.offer())
		if backAt.IsZero() && lowest < evBefore && s.offer() == evBefore {
			backAt = s.now
		}
	}
	back := "never cut"
	if !backAt.IsZero() {
		back = "back after " + backAt.Sub(stepAt).String()
	}
	t.Logf("EV %.0f -> lowest %.0f W, %s; battery %.0f -> %.0f W", evBefore, lowest, back, batteryBefore, s.batteryW)
	if lowest <= 0 {
		t.Fatal("the house step stopped the car")
	}
	if s.batteryW > batteryBefore-2000+.01 {
		t.Fatalf("battery %.0f -> %.0f W did not yield the step", batteryBefore, s.batteryW)
	}
	if lowest < evBefore && (backAt.IsZero() || backAt.Sub(stepAt) > time.Minute) {
		t.Fatalf("EV not back at %.0f W within a minute (now %.0f W)", evBefore, s.offer())
	}
	if s.gridW() > 16560.01 {
		t.Fatalf("grid %.0f W above 24 A after the step", s.gridW())
	}
}
