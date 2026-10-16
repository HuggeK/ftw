package loadpoint

import "testing"

// A capacity the car itself reported (OCPP 2.0.1 NotifyEVChargingNeeds) is
// session-scoped exactly like a vehicle profile: it steers SoC inference while
// the car is plugged in, survives a config hot-reload, and reverts on plug-out
// because the next car may be a different one.
func TestSetSessionCapacitySessionScoped(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	m.Observe("a", true, 7000, 0, true)

	if m.SetSessionCapacityWh("nope", 40000) {
		t.Fatal("unknown loadpoint should not apply")
	}
	if m.SetSessionCapacityWh("a", 0) {
		t.Fatal("a car that reported no capacity must not zero the loadpoint's own")
	}
	if !m.SetSessionCapacityWh("a", 40000) {
		t.Fatal("apply failed")
	}

	// 20 kWh delivered on a 40 kWh car = +45 points after charging losses from the 30 % anchor.
	// With the configured 60 kWh it would only be +33.
	m.Observe("a", true, 7000, 20000, true)
	st, _ := m.State("a")
	if st.CurrentSoC < 0.749 || st.CurrentSoC > 0.751 {
		t.Fatalf("SoC should follow the reported 40 kWh capacity, got %v", st.CurrentSoC)
	}

	// Hot-reload mid-session keeps the car's figure, not the config's.
	m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
	cfgs := m.Configs()
	if len(cfgs) != 1 || cfgs[0].VehicleCapacityWh != 40000 {
		t.Fatalf("reported capacity should survive config reload, got %+v", cfgs)
	}

	// Plug-out restores the loadpoint's own capacity.
	m.Observe("a", false, 0, 0, false)
	cfgs = m.Configs()
	if len(cfgs) != 1 || cfgs[0].VehicleCapacityWh != 60000 {
		t.Fatalf("capacity should restore to the configured 60000, got %+v", cfgs)
	}
}

// Measured outranks configured, whichever order the two arrive in: the
// session car's capacity is its owner's estimate, the NotifyEVChargingNeeds
// figure is the car that is actually plugged in. Plug-out restores the car's.
func TestReportedCapacityOutranksTheCars(t *testing.T) {
	leaf := CarBinding{ID: "c1", Name: "Leaf", CapacityWh: 40000}
	t.Run("car first", func(t *testing.T) {
		m := NewManager()
		m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
		m.Observe("a", true, 7000, 0, true)

		m.SetCarBinding("a", leaf)
		m.SetSessionCapacityWh("a", 77000)

		if got := m.Configs()[0].VehicleCapacityWh; got != 77000 {
			t.Fatalf("the car's own capacity should win, got %v", got)
		}
		m.Observe("a", false, 0, 0, false)
		if got := m.Configs()[0].VehicleCapacityWh; got != 40000 {
			t.Fatalf("plug-out should restore the car's 40000, got %v", got)
		}
	})

	t.Run("reported first", func(t *testing.T) {
		m := NewManager()
		m.Load([]Config{{ID: "a", VehicleCapacityWh: 60000, PluginSoC: 0.30}})
		m.Observe("a", true, 7000, 0, true)

		m.SetSessionCapacityWh("a", 77000)
		m.SetCarBinding("a", leaf)

		if got := m.Configs()[0].VehicleCapacityWh; got != 77000 {
			t.Fatalf("a later binding must not overwrite the car's own capacity, got %v", got)
		}
		// The binding still names the car for the UI.
		st, _ := m.State("a")
		if st.VehicleName != "Leaf" || st.CarID != "c1" {
			t.Fatalf("the car should still be named, got %+v", st)
		}
		m.Observe("a", false, 0, 0, false)
		if got := m.Configs()[0].VehicleCapacityWh; got != 40000 {
			t.Fatalf("plug-out should restore the car's 40000, got %v", got)
		}
	})
}
