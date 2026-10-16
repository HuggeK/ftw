package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestLiveFuseBudgetPhysicalBalance(t *testing.T) {
	cases := []struct {
		name                          string
		previousGrid, grid, oldEV, ev float64
		managed, duplicate, stale     bool
		phase                         []float64
		maxCap                        float64
	}{
		{name: "fresh actual overload after low value", previousGrid: 1000, grid: 20000, maxCap: 0},
		{name: "existing managed EV stays in replacement pool", previousGrid: 10000, grid: 10000, oldEV: 8000, ev: 8000, managed: true, maxCap: 15250},
		{name: "EV stopped after high filtered power", previousGrid: 2000, grid: 2000, oldEV: 8000, ev: 0, managed: true, maxCap: 15250},
		{name: "stale managed EV cannot create headroom", previousGrid: 2000, grid: 2000, oldEV: 8000, ev: 8000, managed: true, stale: true, maxCap: 15250},
		{name: "new EV rise after older site sample", previousGrid: 1000, grid: 1000, oldEV: 0, ev: 11000, managed: true, maxCap: 16250},
		{name: "unmanaged EV remains base demand", previousGrid: 8000, grid: 8000, oldEV: 6000, ev: 6000, maxCap: 9250},
		{name: "duplicate identity adds only once", previousGrid: 10000, grid: 10000, oldEV: 8000, ev: 8000, managed: true, duplicate: true, maxCap: 15250},
		{name: "PV export can supply measured net headroom", previousGrid: -5000, grid: -5000, maxCap: 22250},
		{name: "measured battery relief counted once", previousGrid: -3000, grid: -3000, maxCap: 20250},
		{name: "mixed phase directions under net export", previousGrid: -2300, grid: -2300, phase: []float64{10, 10, 10}, maxCap: 10350},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := telemetry.NewStore()
			var data json.RawMessage
			if tc.phase != nil {
				data, _ = json.Marshal(map[string]float64{"l1_a": tc.phase[0], "l2_a": tc.phase[1], "l3_a": tc.phase[2]})
			}
			store.Update("meter", telemetry.DerMeter, tc.previousGrid, nil, data)
			store.Update("meter", telemetry.DerMeter, tc.grid, nil, data)
			store.DriverHealthMut("meter").RecordSuccess()
			store.Update("car", telemetry.DerEV, tc.oldEV, nil, nil)
			store.Update("car", telemetry.DerEV, tc.ev, nil, nil)
			store.DriverHealthMut("car").RecordSuccess()
			if tc.stale {
				store.Get("car", telemetry.DerEV).UpdatedAt = time.Now().Add(-time.Hour)
			}
			st := NewState(0, 0, "meter")
			st.SiteFuseAmps = 25
			st.SiteFuseVoltage = 230
			st.SiteFusePhases = 3
			var points []loadpoint.State
			if tc.managed {
				points = append(points, loadpoint.State{ID: "a", DriverName: "car", PluggedIn: true})
			}
			if tc.duplicate {
				points = append(points, loadpoint.State{ID: "b", DriverName: "car", PluggedIn: true})
			}
			got, known := EVFuseBudget(store, st, points, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p"}}, 17250)
			if !known || got < 0 || got > tc.maxCap+0.001 {
				t.Fatalf("got cap=%v known=%v max=%v; rawgrid=%v smoothgrid=%v rawev=%v smoothev=%v", got, known, tc.maxCap, store.Get("meter", telemetry.DerMeter).RawW, store.Get("meter", telemetry.DerMeter).SmoothedW, store.Get("car", telemetry.DerEV).RawW, store.Get("car", telemetry.DerEV).SmoothedW)
			}
			if tc.maxCap > 0 && !tc.stale && tc.name != "mixed phase directions under net export" && got == 0 {
				t.Fatalf("usable physical budget abandoned: max=%v", tc.maxCap)
			}
		})
	}
}
