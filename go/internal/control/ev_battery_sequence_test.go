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
	wall := time.Now()
	const houseW, desiredEVW = 5000.0, 10350.0
	actualBattery, actualEV := 8000.0, 0.0
	config := loadpoint.Config{ID: "garage", DriverName: "charger", MinChargeW: 4140, MaxChargeW: desiredEVW,
		PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{config})
	store := telemetry.NewStore()
	dir := SlotDirective{SlotStart: wall, SlotEnd: wall.Add(15 * time.Minute), BatteryEnergyWh: 2500, Strategy: "arbitrage"}
	st := newStateWithEnergyDispatch(dir, "meter")
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 1
	st.SlewEnabled = false
	st.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	var sampledPhaseA float64
	controller := loadpoint.NewController(mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: actualEV, PowerAt: time.Now(), SessionWhUnavailable: true}, true
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
	emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64) {
		d := map[string]any{"control_power_available": true, "control_power_observed_at": at.Format(time.RFC3339Nano)}
		if kind == telemetry.DerMeter {
			d["l1_a"], d["l2_a"], d["l3_a"] = sampledPhaseA, sampledPhaseA, sampledPhaseA
		}
		data, _ := json.Marshal(d)
		soc := .5
		store.Update(driver, kind, watts, &soc, data)
		store.RecordDriverSuccess(driver)
	}
	for tick := 0; tick < 6; tick++ {
		if tick == 3 {
			actualBattery = 0
		} // Only now does the battery obey.
		at := wall.Add(-9*time.Second + time.Duration(tick)*1500*time.Millisecond)
		sampledPhaseA = (houseW + actualBattery + actualEV) / 690
		emit("pixii", telemetry.DerBattery, at, actualBattery)
		emit(config.DriverName, telemetry.DerEV, at, actualEV)
		emit("meter", telemetry.DerMeter, at.Add(500*time.Millisecond), houseW+actualBattery+actualEV)
		emit("pixii", telemetry.DerBattery, at.Add(time.Second), actualBattery)
		emit(config.DriverName, telemetry.DerEV, at.Add(time.Second), actualEV)
		controller.TickWithDispatch(context.Background(), wall.Add(time.Duration(tick)*5*time.Second), true)
		st.EVCommandedW = loadpoint.CommandedEVPowerW(mgr.States())
		st.SetEVFuseDemands(controller.FuseDemands())
		targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15000}), 17250)
		if len(targets) != 1 || targets[0].TargetW > 1210.001 {
			t.Fatalf("tick %d: battery did not yield to the EV: %+v", tick, targets)
		}
		if tick < 4 && actualEV != 0 {
			t.Fatalf("tick %d: EV resumed before measured room was stable: %v W", tick, actualEV)
		}
		if tick >= 4 {
			if math.Abs(actualEV-desiredEVW) > .001 {
				t.Fatalf("tick %d: EV stuck at %v W after recovery", tick, actualEV)
			}
			actualBattery = targets[0].TargetW
			if (houseW+actualBattery+actualEV)/690 > 24.001 {
				t.Fatal("battery took the EV's new headroom")
			}
		}
	}
}

func TestEVBatteryCommandOrdersStayWithinMeasuredHeadroom(t *testing.T) {
	rng := rand.New(rand.NewSource(1667))
	for sample := 0; sample < 300; sample++ {
		house := rng.Float64() * 5000
		first, second := rng.Float64()*4000, rng.Float64()*4000
		ev := rng.Float64() * 3000
		grid := house + first + second + ev
		requestedEV := rng.Float64() * 11040
		// The EV's own guard has allowed only a measured increase. Its new
		// draw may appear before the next site sample or any battery reply.
		actualEV := math.Max(ev, math.Min(requestedEV, ev+16560-grid))
		amps := grid / 690
		store, state := evBatteryFixture(t, grid, []float64{amps, amps, amps},
			evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: ev, after: actualEV, requested: requestedEV, phaseMode: "3p"},
			evBatteryFixturePower{driver: "first", kind: telemetry.DerBattery, before: first, after: first},
			evBatteryFixturePower{driver: "second", kind: telemetry.DerBattery, before: second, after: second})
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

func TestEVBatteryPendingAndNewOrdersShareOnePhysicalBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(1668))
	for sample := 0; sample < 300; sample++ {
		house, first, second, ev := rng.Float64()*3000, rng.Float64()*2000, rng.Float64()*2000, rng.Float64()*2000
		grid := house + first + second + ev
		pendingFirst := first + rng.Float64()*(16560-grid)
		pendingSecond := second + rng.Float64()*(16560-grid-(pendingFirst-first))
		requestedEV := rng.Float64() * 11040
		amps := grid / 690
		store, state := evBatteryFixture(t, grid, []float64{amps, amps, amps},
			evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: ev, after: ev, requested: requestedEV, phaseMode: "3p"},
			evBatteryFixturePower{driver: "first", kind: telemetry.DerBattery, before: first, after: first},
			evBatteryFixturePower{driver: "second", kind: telemetry.DerBattery, before: second, after: second})
		state.DriverLimits = map[string]PowerLimits{"first": {MaxChargeW: 17000, MaxDischargeW: 10000}, "second": {MaxChargeW: 17000, MaxDischargeW: 10000}}
		for driver, watts := range map[string]float64{"first": pendingFirst, "second": pendingSecond} {
			payload, _ := json.Marshal(map[string]any{"action": "battery", "power_w": watts})
			command := store.BeginCommand(driver, payload, time.Now())
			store.CompleteCommand(command, "accepted")
		}
		capW, _ := EVFuseBudget(store, state, []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
		actualEV := math.Max(ev, math.Min(requestedEV, capW))
		out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: rng.Float64() * 10000}, {Driver: "second", TargetW: rng.Float64() * 10000}}, store, state, 17250)
		// Either old order can arrive after either new one. A newer command
		// on the same device replaces its offer, never its pending evidence.
		worst := house + actualEV + math.Max(pendingFirst, out[0].TargetW) + math.Max(pendingSecond, out[1].TargetW)
		if worst > 16560.001 {
			t.Fatalf("sample %d: old/new battery orders and EV reach %.1f W; pending %.1f/%.1f EV %.1f targets %+v", sample, worst, pendingFirst, pendingSecond, actualEV, out)
		}
	}
}
