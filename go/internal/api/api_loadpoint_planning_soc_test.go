package api

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol"
)

// The charging view's "Planning from" line is the level the planner starts
// from. Both now read the loadpoint's level, which the cars runtime anchors
// to the session car's own reading (docs/cars.md), so they cannot differ.
func TestLoadpointPlanningSoCMatchesPlannerChoice(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	readAt := time.Now().Add(-34 * time.Minute)
	mgr.Observe("garage", true, 0, 0, true)
	if !mgr.AnchorVehicleSoCAt("garage", .78, readAt) && !mgr.SeedSessionLevel("garage",
		loadpoint.LevelSeed{Known: true, SoC: .78, Confirmed: true, ConfirmedAt: readAt, FromCar: true}) {
		t.Fatal("anchor")
	}

	views := fakeCars{"garage": cars.SessionView{CarID: "c1", Source: protocol.CarSoCVehicle, At: readAt, SoC: .78}}
	srv := New(&Deps{Loadpoints: mgr, Cars: views})
	states := mgr.States()
	srv.applyPlanningSoC(states, time.Now())
	if states[0].PlanningSoC == nil || math.Abs(*states[0].PlanningSoC-.78) > 1e-9 || !states[0].PlanningFromVehicle {
		t.Fatalf("planning view = %v, want 0.78 from the car", states[0].PlanningSoC)
	}

	// A level someone set is the plan's start too, but not the car's word.
	if !mgr.SetCurrentSoC("garage", .27) {
		t.Fatal("SetCurrentSoC")
	}
	views["garage"] = cars.SessionView{CarID: "c1", Source: protocol.CarSoCManual, SoC: .27}
	states = mgr.States()
	srv.applyPlanningSoC(states, time.Now())
	if states[0].PlanningSoC == nil || math.Abs(*states[0].PlanningSoC-.27) > 1e-9 || states[0].PlanningFromVehicle {
		t.Fatalf("manual planning view = %v fromVehicle=%v", states[0].PlanningSoC, states[0].PlanningFromVehicle)
	}

	// A visitor has no level, so no plan starts from one.
	if !mgr.SeedSessionLevel("garage", loadpoint.LevelSeed{}) {
		t.Fatal("visitor seed")
	}
	states = mgr.States()
	srv.applyPlanningSoC(states, time.Now())
	if states[0].PlanningSoC != nil {
		t.Fatalf("visitor planning view = %v, want none", *states[0].PlanningSoC)
	}
}
