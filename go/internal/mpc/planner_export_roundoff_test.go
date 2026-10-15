package mpc

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidatePlanExportPolicyDistinguishesIdleResidueFromDischarge(t *testing.T) {
	for _, mode := range []Mode{ModeSelfConsumption, ModeArbitrage, ModePassiveArbitrage, ModeCheapCharge} {
		for _, blocksBattery := range []bool{false, true} {
			for _, tc := range []struct {
				name     string
				batteryW float64
				evW      float64
				reject   bool
			}{
				{"idle battery", 0, 1400, false},
				{"battery residue", -0.000001, 1400, false},
				{"battery residue limit", -0.00001, 1400, false},
				{"EV residue", -0.01, 0.000001, false},
				{"EV residue limit", -0.01, 0.00001, false},
				{"battery outside tolerance", -0.000011, 1400, true},
				{"EV outside tolerance", -0.01, 0.000011, true},
				{"one watt discharge", -1, 1400, true},
				{"large discharge", -500, 1400, true},
			} {
				t.Run(fmt.Sprintf("%s/blocked=%t/%s", mode, blocksBattery, tc.name), func(t *testing.T) {
					slots, p := nativeFixture()
					slots = slots[:1]
					slots[0].PVW = -5000
					p.Mode = mode
					p.Loadpoint.NoBatteryToEV = blocksBattery
					p.Loadpoint.TargetSoC = 0
					// One hour: replay AC discharge through the cell efficiency.
					gridW := 500 - 5000 + tc.batteryW + tc.evW
					cost := gridW / 1000 * 10 // Published export price is 10 ore/kWh.
					plan := Plan{TotalCostOre: cost, Actions: []Action{{
						SlotStartMs: 1, SlotLenMin: 60,
						BatteryW: tc.batteryW, GridW: gridW,
						SoC: .2 + tc.batteryW/.95/10000, CostOre: cost,
						LoadpointW: tc.evW, LoadpointSoC: .2 + tc.evW*.9/10000,
					}}}
					err := ValidatePlan(slots, p, &plan)
					if tc.reject {
						if err == nil || !strings.Contains(err.Error(), "charges during battery-driven export") {
							t.Fatalf("real battery export with EV charge must fail that policy check: %v", err)
						}
					} else if err != nil {
						t.Fatalf("numeric residue rejected a physically idle action: %v", err)
					}
				})
			}
		}
	}
}
