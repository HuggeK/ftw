package mpc

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

func assertRecoveryPlan(t *testing.T, slots []Slot, p Params, plan Plan) {
	t.Helper()
	if len(slots) == 0 || plan.HorizonSlots != len(slots) {
		t.Fatalf("empty or inconsistent horizon: slots=%d horizon=%d", len(slots), plan.HorizonSlots)
	}
	if err := validatePlanSlotAlignment(slots, plan.Actions); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(plan); err != nil {
		t.Fatalf("plan cannot reach diagnostics or clients: %v", err)
	}
}

func TestCoreRecoveryNonFiniteForecastMatrix(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Slot, float64)
	}{
		{"price", func(s *Slot, v float64) { s.PriceOre = v }},
		{"spot", func(s *Slot, v float64) { s.SpotOre = v }},
		{"pv", func(s *Slot, v float64) { s.PVW = v }},
		{"load", func(s *Slot, v float64) { s.LoadW = v }},
		{"import limit", func(s *Slot, v float64) { s.Limits.MaxImportW = v }},
		{"export limit", func(s *Slot, v float64) { s.Limits.MaxExportW = v }},
	}
	for _, field := range fields {
		for _, value := range []struct {
			name string
			v    float64
		}{{"nan", math.NaN()}, {"+inf", math.Inf(1)}, {"-inf", math.Inf(-1)}} {
			for _, at := range []int{0, 1, 12, 23} {
				t.Run(field.name+"/"+value.name+"/slot"+strconv.Itoa(at), func(t *testing.T) {
					slots, p := recoveryFixture()
					field.set(&slots[at], value.v)
					plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
					if at == 0 {
						if err == nil || len(plan.Actions) != 0 || len(used) != 0 {
							t.Fatalf("invalid current input accepted: slots=%d err=%v", len(used), err)
						}
						return
					}
					if err != nil || len(used) != at {
						t.Fatalf("future bad input lost valid prefix: slots=%d want=%d err=%v", len(used), at, err)
					}
					assertRecoveryPlan(t, used, p, plan)
					if plan.Solver == nil || (plan.Solver.Fallback && !strings.Contains(plan.Solver.FallbackReason, "invalid")) {
						t.Fatalf("invalid input was hidden: %+v", plan.Solver)
					}
				})
			}
		}
	}
}

func TestCoreRecoveryRejectsCorruptPhysics(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Params, float64)
	}{
		{"capacity", func(p *Params, v float64) { p.CapacityWh = v }},
		{"initial soc", func(p *Params, v float64) { p.InitialSoC = v }},
		{"soc min", func(p *Params, v float64) { p.SoCMin = v }},
		{"soc max", func(p *Params, v float64) { p.SoCMax = v }},
		{"charge power", func(p *Params, v float64) { p.MaxChargeW = v }},
		{"discharge power", func(p *Params, v float64) { p.MaxDischargeW = v }},
		{"charge efficiency", func(p *Params, v float64) { p.ChargeEfficiency = v }},
		{"discharge efficiency", func(p *Params, v float64) { p.DischargeEfficiency = v }},
		{"ev capacity", func(p *Params, v float64) { p.Loadpoint.CapacityWh = v }},
		{"ev soc", func(p *Params, v float64) { p.Loadpoint.InitialSoC = v }},
		{"ev target", func(p *Params, v float64) { p.Loadpoint.TargetSoC = v }},
		{"ev charge power", func(p *Params, v float64) { p.Loadpoint.MaxChargeW = v }},
		{"ev efficiency", func(p *Params, v float64) { p.Loadpoint.ChargeEfficiency = v }},
		{"ev step", func(p *Params, v float64) { p.Loadpoint.AllowedStepsW[1] = v }},
	}
	for _, field := range fields {
		for _, value := range []struct {
			name string
			v    float64
		}{{"nan", math.NaN()}, {"+inf", math.Inf(1)}, {"-inf", math.Inf(-1)}, {"negative", -1}} {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				slots, p := recoveryFixture()
				field.set(&p, value.v)
				plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
				if err == nil || len(used) != 0 || len(plan.Actions) != 0 {
					t.Fatalf("corrupt physics became a plan: slots=%d err=%v", len(used), err)
				}
			})
		}
	}
}

func TestCoreRecoveryInvalidTimelineAndBounds(t *testing.T) {
	for _, mutation := range []struct {
		name string
		set  func([]Slot, int)
	}{
		{"negative load", func(s []Slot, i int) { s[i].LoadW = -1 }},
		{"positive pv", func(s []Slot, i int) { s[i].PVW = 1 }},
		{"negative import limit", func(s []Slot, i int) { s[i].Limits.MaxImportW = -1 }},
		{"negative export limit", func(s []Slot, i int) { s[i].Limits.MaxExportW = -1 }},
		{"zero length", func(s []Slot, i int) { s[i].LenMin = 0 }},
		{"negative length", func(s []Slot, i int) { s[i].LenMin = -15 }},
		{"overflow length", func(s []Slot, i int) { s[i].LenMin = math.MaxInt }},
		{"overflow end", func(s []Slot, i int) { s[i].StartMs = math.MaxInt64 - 1 }},
		{"zero start", func(s []Slot, i int) { s[i].StartMs = 0 }},
		{"expired execution", func(s []Slot, i int) { s[i].ExecutionStartMs = s[i].StartMs + 15*60000 }},
	} {
		for _, at := range []int{0, 12} {
			t.Run(mutation.name+"/slot"+strconv.Itoa(at), func(t *testing.T) {
				slots, p := recoveryFixture()
				mutation.set(slots, at)
				plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
				if at == 0 || strings.Contains(mutation.name, "length") || strings.Contains(mutation.name, "start") || mutation.name == "overflow end" || mutation.name == "expired execution" {
					if err == nil || len(used) != 0 || len(plan.Actions) != 0 {
						t.Fatalf("invalid current interval accepted: %+v err=%v", used, err)
					}
					return
				}
				if err != nil || len(used) != at {
					t.Fatalf("invalid future interval lost earlier plan: slots=%d err=%v", len(used), err)
				}
				assertRecoveryPlan(t, used, p, plan)
			})
		}
	}
	for _, slots := range [][]Slot{nil, {}} {
		_, p := recoveryFixture()
		if plan, used, err := corePlanWithRecovery(context.Background(), slots, p); err == nil || len(plan.Actions) != 0 || len(used) != 0 {
			t.Fatal("an empty input became a plan")
		}
	}
}

func TestCoreRecoveryAllocationOverflow(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("oversized resolution panicked instead of using the reserve: %v", r)
		}
	}()
	for _, dimension := range []string{"actions", "battery", "ev"} {
		slots, p := recoveryFixture()
		switch dimension {
		case "actions":
			p.Loadpoint, p.ActionLevels = nil, math.MaxInt
		case "battery":
			p.Loadpoint, p.SoCLevels = nil, math.MaxInt
		case "ev":
			p.Loadpoint.TargetSoC, p.Loadpoint.Levels = 0, math.MaxInt
		}
		plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
		if err != nil || len(used) != len(slots) {
			t.Fatalf("oversized %s resolution lost safe plan: slots=%d err=%v", dimension, len(used), err)
		}
		assertRecoveryPlan(t, used, p, plan)
		if plan.Solver == nil || !plan.Solver.Fallback || !strings.Contains(plan.Solver.FallbackReason, "memory budget") {
			t.Fatalf("resource fallback was hidden: %+v", plan.Solver)
		}
	}
}

func TestCoreRecoveryImpossibleGoalsKeepSafeActions(t *testing.T) {
	for _, mutation := range []struct {
		name string
		set  func([]Slot, *Params)
	}{
		{"empty battery", func(_ []Slot, p *Params) { p.InitialSoC = 0 }},
		{"full battery", func(_ []Slot, p *Params) { p.InitialSoC = 1 }},
		{"zero battery power", func(_ []Slot, p *Params) { p.MaxChargeW, p.MaxDischargeW = 0, 0 }},
		{"unreachable departure", func(_ []Slot, p *Params) {
			p.Loadpoint.InitialSoC, p.Loadpoint.TargetSlotIdx = 0, 0
		}},
		{"surplus only without sun", func(_ []Slot, p *Params) { p.Loadpoint.SurplusOnly = true }},
		{"no charger power", func(_ []Slot, p *Params) { p.Loadpoint.AllowedStepsW = []float64{0} }},
		{"negative prices", func(s []Slot, _ *Params) {
			for i := range s {
				s[i].PriceOre, s[i].SpotOre = -1000, -1100
			}
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			slots, p := recoveryFixture()
			mutation.set(slots, &p)
			plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
			if err != nil || len(used) != len(slots) {
				t.Fatalf("unreachable goal lost safe plan: slots=%d err=%v", len(used), err)
			}
			assertRecoveryPlan(t, used, p, plan)
			if mutation.name == "unreachable departure" || mutation.name == "surplus only without sun" || mutation.name == "no charger power" {
				if plan.LoadpointShortfallWh[p.Loadpoint.ID] <= 0 {
					t.Fatal("unreachable goal was reported as met")
				}
			}
		})
	}
}

// The generated first interval always has a known-safe idle action. Later
// intervals may exceed power or energy limits. Recovery must retain at least
// that first interval, or the complete horizon when the idle witness is safe.
func TestCoreRecoveryGeneratedForecastFaults(t *testing.T) {
	rng := rand.New(rand.NewPCG(1415, 1631))
	for trial := range 300 {
		slots, p := recoveryFixture()
		p.InitialSoC = p.SoCMin + rng.Float64()*(p.SoCMax-p.SoCMin)
		p.Mode = []Mode{ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage, ModeArbitrage}[trial%4]
		if trial%2 == 0 {
			p.Loadpoint = nil
		}
		idleFeasible := true
		for i := range slots {
			if i == 0 {
				continue
			}
			maxPower := 40000.0
			if trial%3 == 0 {
				maxPower = 17000 // idle is a witness for the complete horizon
			}
			slots[i].PVW, slots[i].LoadW = -rng.Float64()*maxPower, rng.Float64()*maxPower
			net := slots[i].PVW + slots[i].LoadW
			idleFeasible = idleFeasible && math.Abs(net) <= slots[i].Limits.MaxImportW
		}
		plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
		if err != nil {
			t.Fatalf("trial %d lost safe first interval: %v", trial, err)
		}
		assertRecoveryPlan(t, used, p, plan)
		if idleFeasible && len(used) != len(slots) {
			t.Fatalf("trial %d lost known-safe idle horizon: slots=%d", trial, len(used))
		}
	}
}

func FuzzCoreRecoveryNumericInputs(f *testing.F) {
	for field := uint8(0); field < 12; field++ {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64, -1, 0, .5} {
			f.Add(math.Float64bits(value), field, uint8(0))
			f.Add(math.Float64bits(value), field, uint8(12))
		}
	}
	f.Fuzz(func(t *testing.T, bits uint64, field, position uint8) {
		slots, p := recoveryFixture()
		v, at := math.Float64frombits(bits), int(position)%len(slots)
		switch field % 12 {
		case 0:
			slots[at].LoadW = v
		case 1:
			slots[at].PVW = v
		case 2:
			slots[at].PriceOre = v
		case 3:
			slots[at].SpotOre = v
		case 4:
			slots[at].Limits.MaxImportW = v
		case 5:
			slots[at].Limits.MaxExportW = v
		case 6:
			p.CapacityWh = v
		case 7:
			p.InitialSoC = v
		case 8:
			p.ChargeEfficiency = v
		case 9:
			p.DischargeEfficiency = v
		case 10:
			p.Loadpoint.InitialSoC = v
		case 11:
			p.Loadpoint.AllowedStepsW[1] = v
		}
		plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
		if err != nil {
			if len(plan.Actions) != 0 || len(used) != 0 {
				t.Fatal("failed solve leaked a partial candidate")
			}
			if field%12 < 6 && at > 0 {
				t.Fatalf("future numeric fault lost safe current interval: %v", err)
			}
			return
		}
		assertRecoveryPlan(t, used, p, plan)
	})
}
