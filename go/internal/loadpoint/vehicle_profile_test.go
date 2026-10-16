package loadpoint

import "testing"

// The session's car brings its capacity (docs/cars.md). Before cars a
// vehicle profile did this for one session and plug-out reverted it; now the
// cars runtime binds a car to each loadpoint and the binding holds until the
// runtime changes it, across a config reload too.
func TestCarBindingSteersTheSessionsCapacity(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	m.Observe("a", true, 7000, 0, true)

	if m.SetCarBinding("nope", CarBinding{ID: "c1", CapacityWh: 40000}) {
		t.Fatal("unknown loadpoint should not apply")
	}
	if !m.SetCarBinding("a", CarBinding{ID: "c1", Name: "Leaf", CapacityWh: 40000, IdentifiedBy: "tap"}) {
		t.Fatal("apply failed")
	}
	st, _ := m.State("a")
	if st.VehicleName != "Leaf" || st.CarID != "c1" || st.CarIdentifiedBy != "tap" {
		t.Fatalf("car missing from state: %+v", st)
	}

	// 20 kWh delivered on a 40 kWh car = +45 points after charging losses
	// from the 30 % anchor. With the configured 60 kWh it would only be +33.
	m.Observe("a", true, 7000, 20000, true)
	st, _ = m.State("a")
	if st.CurrentSoC < 0.749 || st.CurrentSoC > 0.751 {
		t.Fatalf("SoC should follow the car's 40 kWh capacity, got %v", st.CurrentSoC)
	}

	// Hot-reload mid-session: the car survives.
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	if st, _ = m.State("a"); st.VehicleName != "Leaf" || st.VehicleCapacityWh != 40000 {
		t.Fatalf("car should survive config reload, got %+v", st)
	}

	// Plug-out leaves the car the charger last had; the charger shows no
	// session car until the next plug-in.
	m.Observe("a", false, 0, 0, false)
	if st, _ = m.State("a"); st.CarID != "" || st.VehicleCapacityWh != 40000 {
		t.Fatalf("after plug-out: %+v", st)
	}
}

// A visitor's session has no level: the loadpoint counts only what it
// delivers and says it does not know the car's.
func TestAVisitorsSessionHasNoLevel(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	m.Observe("a", true, 7000, 0, true)
	m.SetCarBinding("a", CarBinding{IdentifiedBy: "none"})
	if !m.SeedSessionLevel("a", LevelSeed{}) {
		t.Fatal("seed")
	}
	st, _ := m.State("a")
	if !st.SoCUnknown || st.SoCSource != "unknown" || st.CurrentSoC != 0 || st.VehicleCapacityWh != 60000 {
		t.Fatalf("visitor %+v", st)
	}
	// Someone who knows sets it; then it is known.
	if !m.SetCurrentSoC("a", .4) {
		t.Fatal("set")
	}
	if st, _ = m.State("a"); st.SoCUnknown || st.CurrentSoC != .4 {
		t.Fatalf("after a level was set %+v", st)
	}
}
