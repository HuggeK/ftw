package control

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type evBatteryFixturePower struct {
	driver                   string
	kind                     telemetry.DerType
	before, after, requested float64
	phaseMode                string
	missingBefore            bool
}

func evBatteryFixture(t *testing.T, gridW float64, phaseA []float64, powers ...evBatteryFixturePower) (*telemetry.Store, *State) {
	t.Helper()
	wall := time.Now()
	store := telemetry.NewStore()
	state := NewState(0, 0, "meter")
	state.Mode, state.UseEnergyDispatch = ModePlannerArbitrage, true
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
	state.SlewRateW, state.MinDispatchIntervalS = 100000, 0
	emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64, phases []float64) {
		data := map[string]any{"control_power_available": true, "control_power_observed_at": at.Format(time.RFC3339Nano)}
		if phases != nil {
			for i, key := range []string{"l1_a", "l2_a", "l3_a"} {
				if i < len(phases) {
					data[key] = phases[i]
				}
			}
		}
		raw, _ := json.Marshal(data)
		var soc *float64
		if kind == telemetry.DerBattery {
			level := .5
			soc = &level
		}
		store.Update(driver, kind, watts, soc, raw)
		store.RecordDriverSuccess(driver)
	}
	for _, power := range powers {
		if !power.missingBefore {
			emit(power.driver, power.kind, wall.Add(-3*time.Second), power.before, nil)
		}
		if power.requested > 0 {
			state.evFuseDemands = append(state.evFuseDemands, evFuseDemand{power.driver, power.requested, power.phaseMode})
		}
	}
	emit("meter", telemetry.DerMeter, wall.Add(-2*time.Second), gridW, phaseA)
	for _, power := range powers {
		emit(power.driver, power.kind, wall.Add(-time.Second), power.after, nil)
	}
	return store, state
}

func TestEVBatteryNewEVFeedbackCannotFundBatteryCharge(t *testing.T) {
	store, state := evBatteryFixture(t, 6900, []float64{10, 10, 10},
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 9660, requested: 10350, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 3900, after: 3900})
	now := time.Now()
	dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 2500,
		LoadpointEnergyWh: map[string]float64{"car": 2587.5}, LoadpointMaxPowerW: map[string]float64{"car": 10350}, Strategy: "arbitrage"}
	state.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	state.EVCommandedW = 9660
	out := ComputeDispatch(store, state, map[string]float64{"battery": 10000}, 17250)
	if len(out) != 1 || math.Abs(out[0].TargetW-3210) > .1 {
		t.Fatalf("battery took EV room from newer feedback: %v", out)
	}
	if grid := 3000 + 9660 + out[0].TargetW; grid > 16560.01 {
		t.Fatalf("physical grid %.0fW exceeds working ceiling", grid)
	}
}

func TestEVBatterySteadyChargingKeepsMeasuredRoom(t *testing.T) {
	for _, ev := range []float64{0, 4000, 9660} {
		t.Run(fmt.Sprintf("%.0fW", ev), func(t *testing.T) {
			grid := 3000 + 3900 + ev
			amps := grid / 690
			store, state := evBatteryFixture(t, grid, []float64{amps, amps, amps},
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: ev, after: ev, requested: 9660, phaseMode: "3p"},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 3900, after: 3900})
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 3900}}, store, state, 17250)
			if math.Abs(out[0].TargetW-3900) > .01 {
				t.Fatalf("steady/starting EV %.0fW unnecessarily lost battery room: %v", ev, out)
			}
		})
	}
}

func TestEVBatteryLaterBatteryPowerCannotBeReplacementCredit(t *testing.T) {
	store, state := evBatteryFixture(t, 7000, []float64{7000.0 / 690, 7000.0 / 690, 7000.0 / 690},
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 10000})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
	if math.Abs(out[0].TargetW-9560) > .01 {
		t.Fatalf("later battery feedback created extra charge room: %v", out)
	}
}

func TestEVBatteryMissingBracketUsesRemainingRoom(t *testing.T) {
	store, state := evBatteryFixture(t, 3000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, after: 2000, missingBefore: true})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 5000}}, store, state, 17250)
	if out[0].TargetW != 5000 {
		t.Fatalf("missing positive replacement credit blocked spare room: %v", out)
	}
}

func TestEVBatteryOneBatteryReductionDoesNotFundSiblingIncrease(t *testing.T) {
	store, state := evBatteryFixture(t, 14000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "first", kind: telemetry.DerBattery, before: 9000, after: 9000},
		evBatteryFixturePower{driver: "second", kind: telemetry.DerBattery, before: 0, after: 0})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 0}, {Driver: "second", TargetW: 10000}}, store, state, 17250)
	if out[0].TargetW != 0 || math.Abs(out[1].TargetW-2560) > .01 {
		t.Fatalf("unmeasured sibling reduction financed increase: %v", out)
	}
	// Before the first battery has reduced, the second can already apply.
	if 1000+4000+9000+out[1].TargetW > 16560.01 {
		t.Fatal("command ordering can cross the working ceiling")
	}
}

func TestEVBatteryFutureDischargeDoesNotFundCharge(t *testing.T) {
	store, state := evBatteryFixture(t, 7000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "first", kind: telemetry.DerBattery, before: 0, after: 0},
		evBatteryFixturePower{driver: "second", kind: telemetry.DerBattery, before: 0, after: 0})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: -9000}, {Driver: "second", TargetW: 10000}}, store, state, 17250)
	if out[0].TargetW != -9000 || math.Abs(out[1].TargetW-9560) > .01 {
		t.Fatalf("future discharge financed positive offer: %v", out)
	}
}

func TestEVBatteryStoppingDischargeReservesItsLostSupply(t *testing.T) {
	store, state := evBatteryFixture(t, 2000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: -5000, after: -5000})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
	if math.Abs(out[0].TargetW-9560) > .01 {
		t.Fatalf("stopped discharge still funded charge: %v", out)
	}
}

func TestEVBatteryUnmanagedEVDoesNotFundAnotherCar(t *testing.T) {
	store, state := evBatteryFixture(t, 8000, nil,
		evBatteryFixturePower{driver: "other-car", kind: telemetry.DerEV, before: 5000, after: 5000},
		evBatteryFixturePower{driver: "new-car", kind: telemetry.DerEV, before: 0, after: 0, requested: 5000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 2000, after: 2000})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
	if math.Abs(out[0].TargetW-5560) > .01 {
		t.Fatalf("unmanaged EV was used as another car's replacement credit: %v", out)
	}
}

func TestEVBatteryPendingSinglePhaseOfferUsesWorstPhase(t *testing.T) {
	for _, mode := range []string{"1p", "auto", ""} {
		t.Run(mode, func(t *testing.T) {
			store, state := evBatteryFixture(t, 6900, []float64{10, 10, 10},
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 2300, phaseMode: mode},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 3900, after: 3900})
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
			if math.Abs(out[0].TargetW-6660) > .01 {
				t.Fatalf("phase %q offer allowed too much balanced battery charge: %v", mode, out)
			}
		})
	}
}

func TestEVBatteryMixedPhaseSignsDoNotDoubleCreditPV(t *testing.T) {
	store, state := evBatteryFixture(t, -2300, []float64{10, 10, 10},
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 2300, phaseMode: "1p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 0})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
	if math.Abs(out[0].TargetW-2760) > .01 {
		t.Fatalf("net export hid an importing phase: %v", out)
	}
}

func TestEVBatteryUnknownMeterClampsOnlyPositiveOffers(t *testing.T) {
	store, state := evBatteryFixture(t, 0, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 11000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 0})
	store.Remove("meter")
	in := []DispatchTarget{{Driver: "battery", TargetW: 5000}, {Driver: "other", TargetW: -1000}}
	out := clampEVBatteryHeadroom(in, store, state, 17250)
	if out[0].TargetW != 0 || out[1].TargetW != -1000 || in[0].TargetW != 5000 {
		t.Fatalf("unknown site invented headroom, removed discharge, or mutated input: in=%v out=%v", in, out)
	}
}

func TestEVBatteryKeepsChargeNeededForMeasuredExportRelief(t *testing.T) {
	store, state := evBatteryFixture(t, -20000, []float64{20000.0 / 690, 20000.0 / 690, 20000.0 / 690},
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 10000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 0})
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 5000}}, store, state, 17250)
	if out[0].TargetW != 5000 {
		t.Fatalf("EV reserve removed export relief needed before the charger can start: %v", out)
	}
	if -20000+out[0].TargetW < -16560.01 {
		t.Fatal("EV reserve left the measured site above the export ceiling")
	}
}

func TestEVBatteryPendingSiblingOrderCannotFundNewCharge(t *testing.T) {
	for _, includeSibling := range []bool{true, false} {
		t.Run(fmt.Sprint(includeSibling), func(t *testing.T) {
			store, state := evBatteryFixture(t, 1000, nil,
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 4000, phaseMode: "3p"},
				evBatteryFixturePower{driver: "first", kind: telemetry.DerBattery, before: 0, after: 0},
				evBatteryFixturePower{driver: "second", kind: telemetry.DerBattery, before: 0, after: 0})
			// A higher order may still arrive after this tick's lower one.
			now := time.Now()
			old := store.BeginCommand("first", []byte(`{"action":"battery","power_w":8000}`), now.Add(-time.Second))
			store.CompleteCommand(old, "accepted")
			lower := store.BeginCommand("first", []byte(`{"action":"battery","power_w":0}`), now)
			store.CompleteCommand(lower, "accepted")
			in := []DispatchTarget{{Driver: "second", TargetW: 10000}}
			if includeSibling {
				in = append([]DispatchTarget{{Driver: "first", TargetW: 0}}, in...)
			}
			out := clampEVBatteryHeadroom(in, store, state, 17250)
			var got float64
			for _, target := range out {
				if target.Driver == "second" {
					got = target.TargetW
				}
			}
			if math.Abs(got-3560) > .01 {
				t.Fatalf("old sibling order financed a new battery: %+v", out)
			}
			if 1000+4000+8000+got > 16560.01 {
				t.Fatal("EV plus old and new battery orders exceed one physical budget")
			}
		})
	}
}

func TestEVBatteryOwnPendingOrderIsNotCountedTwice(t *testing.T) {
	store, state := evBatteryFixture(t, 1000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 0})
	order := store.BeginCommand("battery", []byte(`{"action":"battery","power_w":8000}`), time.Now())
	store.CompleteCommand(order, "accepted")
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 8000}}, store, state, 17250)
	if out[0].TargetW != 8000 {
		t.Fatalf("same physical offer was reserved twice: %v", out)
	}
}

func TestEVBatteryPollBeforeSiteKeepsSteadyCharge(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		offlineSibling, removed, cold bool
	}{
		{name: "steady"},
		{name: "offline_sibling", offlineSibling: true},
		{name: "removed_driver", removed: true},
		{name: "cold_start", cold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wall := time.Now()
			start := wall.Add(-6 * time.Second)
			store := telemetry.NewStore()
			state := NewState(0, 0, "meter")
			state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases, state.SiteFuseSafetyA = 25, 230, 3, 1
			state.evFuseDemands = []evFuseDemand{{"car", 10350, "3p"}}
			state.clock = func() time.Time { return wall.Add(time.Second) }
			if tc.offlineSibling {
				state.DriverLimits = map[string]PowerLimits{"offline": {MaxChargeW: 10000, MaxDischargeW: 10000}}
			}
			if tc.removed {
				state.PrevTargets = map[string]float64{"removed": 10000}
			}
			emit := func(driver string, kind telemetry.DerType, at time.Time, watts float64) {
				data := map[string]any{"control_power_available": true, "control_power_observed_at": at.Format(time.RFC3339Nano)}
				if kind == telemetry.DerMeter {
					for _, key := range []string{"l1_a", "l2_a", "l3_a"} {
						data[key] = watts / 690
					}
				}
				raw, _ := json.Marshal(data)
				store.Update(driver, kind, watts, nil, raw)
				store.RecordDriverSuccess(driver)
			}
			emit("battery", telemetry.DerBattery, start.Add(-1900*time.Millisecond), 1210)
			emit("car", telemetry.DerEV, start.Add(-1900*time.Millisecond), 10350)
			if !tc.cold {
				emit("meter", telemetry.DerMeter, start.Add(-1500*time.Millisecond), 16560)
			}
			emit("car", telemetry.DerEV, start.Add(-1400*time.Millisecond), 10350)
			batteryW := 1210.0
			var commands []float64
			for tick := range 6 {
				at := start.Add(time.Duration(tick) * time.Second)
				emit("car", telemetry.DerEV, at.Add(-300*time.Millisecond), 10350)
				emit("battery", telemetry.DerBattery, at.Add(-200*time.Millisecond), batteryW)
				emit("meter", telemetry.DerMeter, at, 5000+10350+batteryW)
				emit("car", telemetry.DerEV, at.Add(100*time.Millisecond), 10350)
				out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 1210}}, store, state, 17250)
				batteryW = out[0].TargetW
				commands = append(commands, batteryW)
			}
			for tick, watts := range commands {
				if tc.cold && tick < 2 {
					continue // No common source observation yet.
				}
				if math.Abs(watts-1210) > .01 {
					t.Fatalf("steady source poll order needlessly changed battery at tick%d: %v", tick, commands)
				}
			}
			t.Logf("battery commands: %v", commands)
		})
	}
}

func TestEVBatteryUsesQualifiedACInsteadOfRawDC(t *testing.T) {
	store, state := evBatteryFixture(t, 13000, nil,
		evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
		evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 8000, after: 8000})
	at := time.Now().Add(-500 * time.Millisecond)
	data, _ := json.Marshal(map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano), "control_power_w": 8000})
	store.Update("battery", telemetry.DerBattery, 2000, nil, data)
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 8000}}, store, state, 17250)
	if out[0].TargetW != 8000 {
		t.Fatalf("raw DC replaced qualified AC in site balance: %v", out)
	}
}

func TestEVBatteryObservedSiblingRiseNeedsRoomWithoutOrder(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		beforeW, afterW, gridW, wantW float64
	}{
		{"charging", 0, 8000, 7000, 1560},
		{"stopped_discharge", -8000, 0, -1000, 9560},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := evBatteryFixture(t, tc.gridW, nil,
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 4000, after: 4000, requested: 4000, phaseMode: "3p"},
				evBatteryFixturePower{driver: "sibling", kind: telemetry.DerBattery, before: tc.beforeW, after: tc.afterW},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 0, after: 0})
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 10000}}, store, state, 17250)
			if math.Abs(out[0].TargetW-tc.wantW) > .01 || 7000+tc.afterW+out[0].TargetW > 16560.01 {
				t.Fatalf("newer measured sibling rise was absent from the physical budget: %v, want%.0fW", out, tc.wantW)
			}
		})
	}
}

func TestEVBatteryPendingSiblingBacksOffExistingChargeForEV(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hasTarget bool
		targetW   float64
	}{
		{name: "absent"},
		{name: "zero", hasTarget: true},
		{name: "partial", hasTarget: true, targetW: 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := evBatteryFixture(t, 5000, nil,
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 0, after: 0, requested: 5000, phaseMode: "3p"},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 4000, after: 4000},
				evBatteryFixturePower{driver: "sibling", kind: telemetry.DerBattery, before: 0, after: 0})
			old := store.BeginCommand("sibling", []byte(`{"action":"battery","power_w":8000}`), time.Now())
			store.CompleteCommand(old, "accepted")
			targets := []DispatchTarget{{Driver: "battery", TargetW: 4000}}
			if tc.hasTarget {
				targets = append(targets, DispatchTarget{Driver: "sibling", TargetW: tc.targetW})
			}
			out := clampEVBatteryHeadroom(targets, store, state, 17250)
			if math.Abs(out[0].TargetW-2560) > .01 || 1000+5000+8000+out[0].TargetW > 16560.01 {
				t.Fatalf("pending sibling kept optional battery ahead of EV: %v", out)
			}
		})
	}
}

func TestEVBatteryPositiveLimitsUseOneSnapshot(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			targets := []DispatchTarget{{Driver: "first", TargetW: 8000}, {Driver: "second", TargetW: 8000}}
			if reverse {
				targets[0], targets[1] = targets[1], targets[0]
			}
			out := capBatteryOffersToHeadroom(targets, map[string]float64{"first": 0, "second": 0}, map[string]float64{"first": 0, "second": 0}, 10000)
			if out[0].TargetW != 5000 || out[1].TargetW != 5000 {
				t.Fatalf("one target's clamp changed the other's shared allocation: %v", out)
			}
		})
	}
}

func TestEVBatteryOlderBalanceKeepsLatestGridAndPhaseLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gridW, worstA float64
		wantW         float64
	}{
		{"grid_jump", 17560, 17560.0 / 690, 210},
		{"phase_jump", 16560, 25, 520},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := evBatteryFixture(t, 16560, []float64{24, 24, 24},
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: 10350, after: 10350, requested: 10350, phaseMode: "3p"},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: 1210, after: 1210})
			at := time.Now().Add(-500 * time.Millisecond)
			data, _ := json.Marshal(map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano),
				"l1_a": tc.worstA, "l2_a": 24, "l3_a": 24})
			store.Update("meter", telemetry.DerMeter, tc.gridW, nil, data)
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 1210}}, store, state, 17250)
			if math.Abs(out[0].TargetW-tc.wantW) > .01 {
				t.Fatalf("older balance hid latest %s: %v, want%.0fW", tc.name, out, tc.wantW)
			}
		})
	}
}

func TestEVBatteryOlderBalanceRetainsLaterStop(t *testing.T) {
	for _, kind := range []telemetry.DerType{telemetry.DerEV, telemetry.DerBattery} {
		t.Run(kind.String(), func(t *testing.T) {
			carW, batteryW, oldGridW, newGridW, wantW := 4000.0, 1000.0, 6000.0, 4000.0, 7560.0
			driver, restartedW := "car", carW
			if kind == telemetry.DerBattery {
				carW, batteryW, oldGridW, newGridW, wantW = 3000, 8000, 12000, 9000, 4560
				driver, restartedW = "battery", batteryW
			}
			store, state := evBatteryFixture(t, oldGridW, nil,
				evBatteryFixturePower{driver: "car", kind: telemetry.DerEV, before: carW, after: carW, requested: carW, phaseMode: "3p"},
				evBatteryFixturePower{driver: "battery", kind: telemetry.DerBattery, before: batteryW, after: batteryW})
			wall := time.Now()
			emit := func(driver string, kind telemetry.DerType, ago time.Duration, watts float64) {
				at := wall.Add(-ago)
				data, _ := json.Marshal(map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano)})
				store.Update(driver, kind, watts, nil, data)
			}
			emit(driver, kind, 800*time.Millisecond, 0)
			emit("meter", telemetry.DerMeter, 700*time.Millisecond, newGridW)
			emit(driver, kind, 600*time.Millisecond, restartedW)
			out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "battery", TargetW: 12000}}, store, state, 17250)
			if math.Abs(out[0].TargetW-wantW) > .01 {
				t.Fatalf("older balance lost the observed %s stop: %v, want%.0fW", kind, out, wantW)
			}
		})
	}
}
