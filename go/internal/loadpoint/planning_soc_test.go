package loadpoint

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// manualThenVehicle builds a plugged-in session whose confirmed level is
// 27% at manualAt, and a vag_vehicle report of 78% whose measured age is age.
func manualThenVehicle(t *testing.T, manualAt time.Time, age time.Duration) (*Manager, telemetry.VehiclePick, time.Time) {
	t.Helper()
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	clock := manualAt
	m.SetNowFn(func() time.Time { return clock })
	m.Observe("garage", true, 0, 0, true)
	if !m.SetCurrentSoC("garage", 0.27) {
		t.Fatal("SetCurrentSoC")
	}
	tel := telemetry.NewStore()
	if age >= 0 {
		soc := .78
		tel.Update("vag", telemetry.DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
		tel.EmitMetric("vag", "vehicle_soc_age_s", age.Seconds(), "s", "", "")
		tel.DriverHealthMut("vag").RecordSuccess()
	}
	_, receivedAt, _ := tel.LatestMetric("vag", "vehicle_soc_age_s")
	now := receivedAt
	if now.IsZero() {
		now = manualAt.Add(time.Hour)
	} else {
		now = receivedAt.Add(time.Second)
	}
	pick := telemetry.PlanningVehiclePick(tel, false, now)
	return m, pick, now
}

func TestPlanningStartsAtLaterVehicleSoC(t *testing.T) {
	manualAt := time.Now().Add(-2 * time.Hour).UTC()
	m, pick, _ := manualThenVehicle(t, manualAt, 34*time.Minute)
	if pick.Driver != "vag" || pick.SoC != .78 || !pick.UpdatedAt.After(manualAt) {
		t.Fatalf("34-minute vag reading was not a planning candidate: %+v", pick)
	}
	soc, fromVehicle := m.PlanningStartSoC("garage", pick.SoC, pick.UpdatedAt, true)
	if !fromVehicle || math.Abs(soc-.78) > 1e-9 {
		t.Fatalf("planner start = %v fromVehicle=%v, want 0.78 from the car", soc, fromVehicle)
	}

	// Energy delivered after the measurement moves the start, once.
	clock := pick.UpdatedAt
	m.SetNowFn(func() time.Time { return clock })
	m.Observe("garage", true, 0, 0, true)
	clock = pick.UpdatedAt.Add(10 * time.Minute)
	m.Observe("garage", true, 11000, 2000, true)
	soc, fromVehicle = m.PlanningStartSoC("garage", pick.SoC, pick.UpdatedAt, true)
	want := .78 + 2000*DefaultChargeEfficiency/60000
	if !fromVehicle || math.Abs(soc-want) > 1e-9 {
		t.Fatalf("planner start = %v, want %v (measurement plus energy since)", soc, want)
	}
	soc, fromVehicle = m.PlanningStartSoC("garage", pick.SoC, pick.UpdatedAt, true)
	if !fromVehicle || math.Abs(soc-want) > 1e-9 {
		t.Fatalf("energy counted twice: %v", soc)
	}
}

func TestPlanningKeepsManualWhenVehicleIsOlder(t *testing.T) {
	observed := time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC)
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.20}})
	m.SetNowFn(func() time.Time { return observed.Add(-time.Hour) })
	m.Observe("garage", true, 0, 0, true)
	m.SetNowFn(func() time.Time { return observed.Add(time.Minute) })
	if !m.SetCurrentSoC("garage", 0.27) {
		t.Fatal("SetCurrentSoC")
	}
	soc, fromVehicle := m.PlanningStartSoC("garage", .78, observed, true)
	if fromVehicle || math.Abs(soc-.27) > 1e-9 {
		t.Fatalf("newer manual lost: soc=%v fromVehicle=%v", soc, fromVehicle)
	}
	if m.VehicleSoCSupersedes("garage", observed) {
		t.Fatal("older car measurement must not replace a newer manual level")
	}
}

func TestPlanningKeepsManualWhenVehicleIsBeyondAnchorWindow(t *testing.T) {
	manualAt := time.Now().Add(-3 * time.Hour).UTC()
	m, pick, _ := manualThenVehicle(t, manualAt, telemetry.VehicleAnchorMaxAge+time.Minute)
	if pick.Driver != "" {
		t.Fatalf("reading past the anchor window reached planning: %+v", pick)
	}
	soc, fromVehicle := m.PlanningStartSoC("garage", 0, time.Time{}, false)
	if fromVehicle || math.Abs(soc-.27) > 1e-9 {
		t.Fatalf("planner start = %v fromVehicle=%v, want the manual 0.27", soc, fromVehicle)
	}
}

func TestPlanningKeepsManualWhenNoVehicleDriver(t *testing.T) {
	manualAt := time.Now().Add(-2 * time.Hour).UTC()
	m, pick, _ := manualThenVehicle(t, manualAt, -1)
	if pick.Driver != "" {
		t.Fatalf("no driver still produced a pick: %+v", pick)
	}
	soc, fromVehicle := m.PlanningStartSoC("garage", 0, time.Time{}, false)
	if fromVehicle || math.Abs(soc-.27) > 1e-9 {
		t.Fatalf("planner start = %v fromVehicle=%v, want the manual 0.27", soc, fromVehicle)
	}
}

func TestPlanningReplacesUnconfirmedPluginSoC(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", VehicleCapacityWh: 60000, PluginSoC: 0.27}})
	at := time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC)
	m.SetNowFn(func() time.Time { return at })
	m.Observe("garage", true, 0, 0, true)
	soc, fromVehicle := m.PlanningStartSoC("garage", .78, at.Add(-34*time.Minute), true)
	if !fromVehicle || math.Abs(soc-.78) > 1e-9 {
		t.Fatalf("unconfirmed 27%% stayed in front of the car: soc=%v fromVehicle=%v", soc, fromVehicle)
	}
}
