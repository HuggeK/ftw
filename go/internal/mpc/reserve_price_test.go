package mpc

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func reservePriceFailureFixture() ([]Slot, Params) {
	slots, p := recoveryFixture()
	p.Loadpoint.CapacityWh = 10000
	p.Loadpoint.InitialSoC = .2
	p.Loadpoint.TargetSoC = .8
	p.Loadpoint.TargetSlotIdx = 16 // The actual departure stays beyond the safe prefix.
	for i := range slots {
		slots[i].PriceOre, slots[i].SpotOre = 450, 450
		if i >= 4 {
			slots[i].PriceOre, slots[i].SpotOre = 50, 50
		}
	}
	slots[8].LoadW = 55700
	return slots, p
}

func TestCoreRecoveryReserveUsesCheaperSlotsBeforeShortenedHorizon(t *testing.T) {
	slots, p := reservePriceFailureFixture()
	plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 8 || len(plan.Actions) != 8 {
		t.Fatalf("wrong safe prefix: %d slots, %d actions", len(used), len(plan.Actions))
	}
	if err := ValidatePlan(used, p, &plan); err != nil {
		t.Fatal(err)
	}
	for i, a := range plan.Actions[:4] {
		if a.LoadpointW > 1e-6 {
			t.Fatalf("slot %d charges at 4.50 SEK/kWh despite enough later capacity at 0.50: %.3f W", i, a.LoadpointW)
		}
	}
	if last := plan.Actions[len(plan.Actions)-1].LoadpointSoC; math.Abs(last-.8) > 1e-9 {
		t.Fatalf("moving to cheaper slots lost the exact EV target: %.12f", last)
	}
	if len(plan.LoadpointShortfallWh) != 0 || p.Loadpoint.TargetSlotIdx != 16 {
		t.Fatal("the shortened plan moved the actual departure or reported it missed")
	}
}

func TestNativeFailureRecoveryKeepsPricesBeforeImpossibleSlot(t *testing.T) {
	worker := nativeWorker(t, time.Second)
	t.Cleanup(func() { _ = worker.Close() })
	slots, p := reservePriceFailureFixture()
	if _, err := worker.Optimize(context.Background(), slots, p); err == nil || !strings.Contains(err.Error(), "infeasible") {
		t.Fatalf("bad future forecast did not reproduce native infeasibility with valid prices: %v", err)
	}
	plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 8 || plan.Actions[0].LoadpointW != 0 {
		t.Fatalf("native failure lost the safe cheaper reserve: horizon=%d first=%+v", len(used), plan.Actions[0])
	}
	if err := ValidatePlan(used, p, &plan); err != nil {
		t.Fatal(err)
	}
}

func TestReservePriceChoiceRetainsDepartureAndPhysicalLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func([]Slot, *Params)
		wantFirstW    float64
		wantShortfall float64
	}{
		{"cheap later", func(_ []Slot, _ *Params) {}, 0, 0},
		{"departure now", func(_ []Slot, p *Params) { p.Loadpoint.TargetSlotIdx = 0 }, 2000, 4000},
		{"cheap slot blocked by fuse", func(slots []Slot, _ *Params) { slots[1].LoadW = 3000 }, 2000, 0},
		{"average cannot hide illegal on-power", func(slots []Slot, _ *Params) { slots[1].Limits.MaxImportW = 1500 }, 2000, 0},
		{"partial current interval needed", func(slots []Slot, p *Params) {
			slots[0].ExecutionStartMs = slots[0].StartMs + 50*60000
			p.Loadpoint.TargetSoC = .82
		}, 1200, 0},
		{"negative price without extra energy", func(slots []Slot, p *Params) {
			p.Loadpoint.TargetSoC = .4
			slots[1].PriceOre, slots[1].SpotOre = -50, -50
		}, 0, 0},
		{"solar-only keeps its source", func(slots []Slot, p *Params) {
			p.Loadpoint.TargetSoC = .4
			p.Loadpoint.SurplusOnly = true
			slots[0].PVW = -2500
		}, 2000, 0},
		{"PV opportunity cost beats later import", func(slots []Slot, p *Params) {
			p.Loadpoint.TargetSoC = .4
			slots[0].PVW, slots[0].SpotOre = -2500, 30
		}, 2000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, p := recoveryFixture()
			slots = slots[:4]
			for i := range slots {
				slots[i].StartMs = slots[0].StartMs + int64(i)*3600000
				slots[i].LenMin, slots[i].LoadW = 60, 500
				slots[i].PriceOre, slots[i].SpotOre = 50, 50
				slots[i].Limits = PowerLimits{MaxImportW: 3000, MaxExportW: 3000}
			}
			slots[0].PriceOre, slots[0].SpotOre = 450, 450
			p.MaxChargeW, p.MaxDischargeW = 0, 0
			p.Loadpoint.CapacityWh = 10000
			p.Loadpoint.ChargeEfficiency = 1
			p.Loadpoint.InitialSoC, p.Loadpoint.TargetSoC, p.Loadpoint.SoCMax = .2, .8, 1
			p.Loadpoint.TargetSlotIdx = 3
			p.Loadpoint.MinChargeW, p.Loadpoint.MaxChargeW = 2000, 2000
			p.Loadpoint.AllowedStepsW = []float64{0, 2000}
			tc.change(slots, &p)
			before, _ := json.Marshal(p)
			plan := mustCoreReservePlan(t, slots, p)
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatal(err)
			}
			if math.Abs(plan.Actions[0].LoadpointW-tc.wantFirstW) > 1e-6 {
				t.Fatalf("first EV action %.6f W, want %.6f W", plan.Actions[0].LoadpointW, tc.wantFirstW)
			}
			if tc.name == "cheap later" && math.Abs(plan.TotalCostOre-600) > 1e-6 {
				// House: 0.5 kWh at 4.50 plus 1.5 kWh at 0.50 = 3 SEK.
				// Car: 6 kWh at 0.50 = 3 SEK. The old early charge costs 14 SEK total.
				t.Fatalf("reserve cost %.6f ore, want 600", plan.TotalCostOre)
			}
			if math.Abs(plan.LoadpointShortfallWh[p.Loadpoint.ID]-tc.wantShortfall) > 1e-6 {
				t.Fatalf("real departure shortfall changed: %+v", plan.LoadpointShortfallWh)
			}
			goal := p.Loadpoint.TargetSoC*p.Loadpoint.CapacityWh - tc.wantShortfall
			atDeparture := plan.Actions[p.Loadpoint.TargetSlotIdx].LoadpointSoC * p.Loadpoint.CapacityWh
			if math.Abs(atDeparture-goal) > 1e-6 {
				t.Fatalf("wrong energy at departure: %.6f, want %.6f Wh", atDeparture, goal)
			}
			for i, a := range plan.Actions {
				if a.BatteryW != 0 || a.SoC != p.InitialSoC || (i > p.Loadpoint.TargetSlotIdx && a.LoadpointW != 0) {
					t.Fatalf("price choice changed battery trajectory or charged after departure: %+v", a)
				}
			}
			after, _ := json.Marshal(p)
			if string(before) != string(after) {
				t.Fatal("reserve pricing changed the stored planning goals")
			}
		})
	}
}

func TestReservePriceChoiceDoesNotCreateDemandPeak(t *testing.T) {
	slots, p := recoveryFixture()
	slots = slots[:2]
	for i := range slots {
		slots[i].StartMs = slots[0].StartMs + int64(i)*3600000
		slots[i].LenMin = 60
		slots[i].Limits = PowerLimits{MaxImportW: 10000, MaxExportW: 10000}
	}
	slots[0].LoadW, slots[0].PriceOre, slots[0].SpotOre = 3000, 450, 450
	slots[1].LoadW, slots[1].PriceOre, slots[1].SpotOre = 5000, 400, 400
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	p.Loadpoint.CapacityWh, p.Loadpoint.ChargeEfficiency = 10000, 1
	p.Loadpoint.InitialSoC, p.Loadpoint.TargetSoC, p.Loadpoint.SoCMax = .2, .4, 1
	p.Loadpoint.TargetSlotIdx = 1
	p.Loadpoint.MinChargeW, p.Loadpoint.MaxChargeW = 2000, 2000
	p.Loadpoint.AllowedStepsW = []float64{0, 2000}
	p.DemandCharges = []DemandCharge{{ID: "monthly", PricePerKW: 10000, TopN: 1, AlreadyKW: []float64{5}, Hours: []DemandHour{
		{StartMs: slots[0].StartMs, EndMs: slots[1].StartMs},
		{StartMs: slots[1].StartMs, EndMs: slots[1].StartMs + 3600000},
	}}}
	plan := mustCoreReservePlan(t, slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	// Moving 2 kWh saves 1 SEK in energy but raises the monthly peak from
	// 5 to 7 kW, adding 200 SEK. The energy-only pass must retain the reserve.
	if plan.Actions[0].LoadpointW != 2000 || plan.Actions[1].LoadpointW != 0 || plan.Actions[1].GridW > 5000 {
		t.Fatalf("energy repricing created an avoidable demand peak: %+v", plan.Actions)
	}
}
