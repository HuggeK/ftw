package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestLoadpointPlanningSoCMatchesPlannerChoice(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	manualAt := time.Now().Add(-2 * time.Hour)
	mgr.SetNowFn(func() time.Time { return manualAt })
	mgr.Observe("garage", true, 0, 0, true)
	if !mgr.SetCurrentSoC("garage", 0.27) {
		t.Fatal("SetCurrentSoC")
	}

	tel := telemetry.NewStore()
	soc := .78
	tel.Update("vag", telemetry.DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
	tel.EmitMetric("vag", "vehicle_soc_age_s", (34 * time.Minute).Seconds(), "s", "", "")
	tel.DriverHealthMut("vag").RecordSuccess()
	_, receivedAt, _ := tel.LatestMetric("vag", "vehicle_soc_age_s")
	now := receivedAt.Add(time.Second)

	srv := New(&Deps{Loadpoints: mgr, Tel: tel})
	states := mgr.States()
	srv.applyPlanningSoC(states, now)
	if states[0].PlanningSoC == nil || math.Abs(*states[0].PlanningSoC-.78) > 1e-9 || !states[0].PlanningFromVehicle {
		t.Fatalf("planning view = %+v, want 0.78 from the car", states[0].PlanningSoC)
	}

	tel = telemetry.NewStore()
	tel.Update("vag", telemetry.DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
	tel.EmitMetric("vag", "vehicle_soc_age_s", (telemetry.VehicleAnchorMaxAge + time.Minute).Seconds(), "s", "", "")
	tel.DriverHealthMut("vag").RecordSuccess()
	_, receivedAt, _ = tel.LatestMetric("vag", "vehicle_soc_age_s")
	srv = New(&Deps{Loadpoints: mgr, Tel: tel})
	states = mgr.States()
	srv.applyPlanningSoC(states, receivedAt.Add(time.Second))
	if states[0].PlanningSoC == nil || math.Abs(*states[0].PlanningSoC-.27) > 1e-9 || states[0].PlanningFromVehicle {
		t.Fatalf("stale-beyond-window planning view = %+v fromVehicle=%v", states[0].PlanningSoC, states[0].PlanningFromVehicle)
	}

	srv = New(&Deps{Loadpoints: mgr, Tel: telemetry.NewStore()})
	states = mgr.States()
	srv.applyPlanningSoC(states, now)
	if states[0].PlanningSoC == nil || math.Abs(*states[0].PlanningSoC-.27) > 1e-9 || states[0].PlanningFromVehicle {
		t.Fatalf("no-driver planning view = %+v fromVehicle=%v", states[0].PlanningSoC, states[0].PlanningFromVehicle)
	}
}
