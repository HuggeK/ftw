package loadpoint

import (
	"math"
	"testing"
	"time"
)

// The plan starts from the loadpoint's level. The cars runtime anchors it to
// the session car's own reading when that reading supersedes the level the
// session has (docs/cars.md); before cars, PlanningStartSoC made the same
// choice from a picked reading on each plan. These hold the choice.

// manualSession is a plugged-in session whose level someone set to 27% at
// manualAt.
func manualSession(t *testing.T, manualAt time.Time) (*Manager, *time.Time) {
	t.Helper()
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	clock := manualAt
	m.SetNowFn(func() time.Time { return clock })
	m.Observe("garage", true, 0, 0, true)
	if !m.SetCurrentSoC("garage", 0.27) {
		t.Fatal("SetCurrentSoC")
	}
	return m, &clock
}

func level(t *testing.T, m *Manager) float64 {
	t.Helper()
	st, _ := m.State("garage")
	return st.CurrentSoC
}

// A car reading newer than the level someone set replaces it, and energy
// delivered after the reading moves the level once.
func TestPlanningStartsAtALaterCarReading(t *testing.T) {
	manualAt := time.Now().Add(-2 * time.Hour).UTC()
	m, clock := manualSession(t, manualAt)
	readAt := manualAt.Add(86 * time.Minute)
	*clock = readAt
	m.Observe("garage", true, 0, 0, true)
	*clock = readAt.Add(10 * time.Minute)
	m.Observe("garage", true, 11000, 2000, true)
	if !m.VehicleSoCSupersedes("garage", readAt) || !m.AnchorVehicleSoCAt("garage", .78, readAt) {
		t.Fatal("a newer car reading did not replace the level someone set")
	}
	want := .78 + 2000*DefaultChargeEfficiency/60000
	if got := level(t, m); math.Abs(got-want) > 1e-9 {
		t.Fatalf("level = %v, want %v (the reading plus energy since)", got, want)
	}
	if !m.AnchorVehicleSoCAt("garage", .78, readAt) || math.Abs(level(t, m)-want) > 1e-9 {
		t.Fatalf("energy counted twice: %v", level(t, m))
	}
}

func TestPlanningKeepsManualWhenTheCarReadingIsOlder(t *testing.T) {
	observed := time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC)
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.20}})
	m.SetNowFn(func() time.Time { return observed.Add(-time.Hour) })
	m.Observe("garage", true, 0, 0, true)
	m.SetNowFn(func() time.Time { return observed.Add(time.Minute) })
	if !m.SetCurrentSoC("garage", 0.27) {
		t.Fatal("SetCurrentSoC")
	}
	if m.VehicleSoCSupersedes("garage", observed) {
		t.Fatal("older car measurement must not replace a newer manual level")
	}
	if got := level(t, m); math.Abs(got-.27) > 1e-9 {
		t.Fatalf("newer manual lost: %v", got)
	}
}

// Before cars, a reading older than an hour could not start a plan. A car's
// last reading is its level however old; what ages it is energy delivered
// since and the car leaving a charger, which the cars runtime counts.
func TestAnHourOldReadingNewerThanTheManualLevelStillWins(t *testing.T) {
	manualAt := time.Now().Add(-3 * time.Hour).UTC()
	m, clock := manualSession(t, manualAt)
	readAt := manualAt.Add(time.Hour)
	*clock = manualAt.Add(2*time.Hour + time.Minute)
	m.Observe("garage", true, 0, 0, true)
	if !m.VehicleSoCSupersedes("garage", readAt) {
		t.Fatal("the car's later reading lost to an earlier manual level")
	}
	if !m.AnchorVehicleSoCAt("garage", .78, readAt) {
		t.Fatal("anchor")
	}
	if got := level(t, m); math.Abs(got-.78) > 1e-9 {
		t.Fatalf("level = %v, want the car's 0.78", got)
	}
}

func TestPlanningKeepsManualWithoutACarReading(t *testing.T) {
	m, _ := manualSession(t, time.Now().Add(-2*time.Hour).UTC())
	if got := level(t, m); math.Abs(got-.27) > 1e-9 {
		t.Fatalf("level = %v, want the manual 0.27", got)
	}
}

func TestACarReadingReplacesAnUnconfirmedPlugInLevel(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	at := time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return at })
	m.Observe("garage", true, 0, 0, true)
	readAt := at.Add(-34 * time.Minute)
	if !m.VehicleSoCSupersedes("garage", readAt) {
		t.Fatal("an assumed 27% stayed in front of the car")
	}
	// Read before the plug-in: the cars runtime starts the session there.
	if !m.SeedSessionLevel("garage", LevelSeed{Known: true, SoC: .78, Confirmed: true, ConfirmedAt: readAt, FromCar: true}) {
		t.Fatal("seed")
	}
	if got := level(t, m); math.Abs(got-.78) > 1e-9 {
		t.Fatalf("level = %v, want the car's 0.78", got)
	}
}
