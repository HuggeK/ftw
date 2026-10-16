package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

// VehicleObservations is the trust boundary between vehicle telemetry
// (potentially attacker-controlled) and the cars runtime, which binds each
// driver to one car (docs/cars.md). These tests lock in the bounds, the
// health check and the times a report carries.
//
// Before cars the pickers here ranked every vehicle driver against every
// charger, and a second car's fresher report could win. Which car a report
// belongs to is now the cars runtime's binding; its tests hold that a car
// somewhere else never names this charger's car.

func TestVehicleConnectedRankOrdering(t *testing.T) {
	if VehicleConnectedRank("Charging") <= VehicleConnectedRank("NoPower") {
		t.Error("Charging must outrank NoPower")
	}
	if VehicleConnectedRank("NoPower") <= VehicleConnectedRank("Stopped") {
		t.Error("NoPower must outrank Stopped")
	}
	if VehicleConnectedRank("Stopped") <= VehicleConnectedRank("unknown") {
		t.Error("Stopped must outrank unknown")
	}
	if VehicleConnectedRank("Disconnected") >= 0 {
		t.Error("Disconnected must score negative — no cable")
	}
	if VehicleConnectedRank("Starting") != VehicleConnectedRank("Charging") {
		t.Error("Starting and Charging share top rank — vehicle is engaged")
	}
}

// pushVehicle publishes a DerVehicle reading. soc and limit are 0–1
// fractions (core SI). age moves the reading and its SoC observation back.
func pushVehicle(t *testing.T, s *Store, driver string, soc, limit float64,
	state string, stale bool, age time.Duration) {
	t.Helper()
	stored := soc
	data, _ := json.Marshal(map[string]any{
		"charging_state": state,
		"charge_limit":   limit,
		"stale":          stale,
	})
	s.Update(driver, DerVehicle, 0, &stored, data)
	if age > 0 {
		s.mu.Lock()
		if r := s.readings[key(driver, DerVehicle)]; r != nil {
			agedAt := time.Now().Add(-age)
			r.UpdatedAt = agedAt
			r.SoCUpdatedAt = agedAt
		}
		s.mu.Unlock()
	}
	s.DriverHealthMut(driver).RecordSuccess()
}

func observation(t *testing.T, s *Store, driver string) (VehicleObservation, bool) {
	t.Helper()
	for _, o := range VehicleObservations(s) {
		if o.Driver == driver {
			return o, true
		}
	}
	return VehicleObservation{}, false
}

// Two cars, two drivers: each report is listed under its own driver with
// its own state, and nothing chooses between them. A Disconnected car is
// listed too: the cars runtime reads it as no cable.
func TestVehicleObservationsListEveryDriverAndRankNone(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "garage", 0.50, 0.80, "Stopped", false, 0)
	pushVehicle(t, s, "driveway", 0.30, 0.80, "Charging", false, 0)
	pushVehicle(t, s, "away", 0.70, 0.80, "Disconnected", false, 0)
	got := VehicleObservations(s)
	if len(got) != 3 || got[0].Driver != "away" || got[1].Driver != "driveway" || got[2].Driver != "garage" {
		t.Fatalf("observations %+v", got)
	}
	if got[0].ChargingState != "Disconnected" || got[1].SoC != .30 || got[2].ChargeLimit != .80 {
		t.Fatalf("fields %+v", got)
	}
	if VehicleObservations(nil) != nil {
		t.Fatal("nil store")
	}
}

func TestVehicleObservationsDropInvalidSoC(t *testing.T) {
	s := NewStore()
	// Store.Update validates 0–1 before the row exists, so a lying proxy
	// cannot park 2.5 in core.
	pushVehicle(t, s, "rogue", 2.50, 0.80, "Charging", false, 0)
	pushVehicle(t, s, "rogue2", -0.50, 0.80, "Charging", false, 0)
	if got := VehicleObservations(s); len(got) != 0 {
		t.Errorf("invalid SoC must not be listed, got %+v", got)
	}
}

func TestVehicleObservationsClampLegacyChargeLimit(t *testing.T) {
	s := NewStore()
	soc := 0.50
	data, _ := json.Marshal(map[string]any{"charging_state": "Charging", "charge_limit_pct": 200, "stale": false})
	s.Update("rogue", DerVehicle, 0, &soc, data)
	s.DriverHealthMut("rogue").RecordSuccess()
	o, ok := observation(t, s, "rogue")
	if !ok || o.ChargeLimit != 1 {
		t.Errorf("legacy 200%% charge limit = %+v, want clamped to 1", o)
	}
}

// A report ten minutes old keeps its level and its time: whether the level
// is still exact is the energy rule's to say. Its charging state is not
// live, so it neither wakes the car nor finishes a goal.
func TestAnOldReportKeepsItsTimeAndIsNotLive(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "asleep", 0.50, 0.80, "Charging", false, 10*time.Minute)
	o, ok := observation(t, s, "asleep")
	if !ok || o.SoC != .50 || time.Since(o.At) < 10*time.Minute || o.Live(time.Now()) {
		t.Fatalf("old report %+v", o)
	}
}

func TestACarriedSoCIsNotFreshened(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "asleep", 0.50, 0.80, "Charging", false, 0)
	staleAt := time.Now().Add(-VehicleMaxAge - time.Minute)
	s.mu.Lock()
	s.readings[key("asleep", DerVehicle)].SoCUpdatedAt = staleAt
	s.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"charging_state": "Charging", "charge_limit": 0.80, "soc_fresh": false})
	cachedSoC := 0.50
	s.Update("asleep", DerVehicle, 0, &cachedSoC, data)
	r := s.Get("asleep", DerVehicle)
	if r == nil || !r.SoCUpdatedAt.Equal(staleAt) || !r.UpdatedAt.After(staleAt) {
		t.Fatalf("power-only update changed SoC source time: %+v", r)
	}
	o, ok := observation(t, s, "asleep")
	if !ok || !o.At.Equal(staleAt) || !o.ReportedAt.After(staleAt) || o.Live(time.Now()) {
		t.Fatalf("carried SoC was freshened: %+v", o)
	}
}

func TestADriverMarkedStaleIsNeverLive(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "driver-stale", 0.50, 0.80, "Charging", true, 0)
	o, ok := observation(t, s, "driver-stale")
	if !ok || !o.Stale || o.Live(time.Now()) {
		t.Fatalf("driver-marked stale report: %+v", o)
	}
}

func TestAnOfflineDriverContributesNothing(t *testing.T) {
	s := NewStore()
	soc := .5
	s.Update("offline", DerVehicle, 0, &soc, json.RawMessage(`{"charging_state":"Stopped"}`))
	if got := VehicleObservations(s); len(got) != 0 {
		t.Fatalf("offline driver listed: %+v", got)
	}
}

// After a restart a cloud driver replays its last report with its measured
// age. It is listed at the time the car measured it, never as now, and it is
// never live.
func TestACachedCloudReportKeepsTheTimeTheCarMeasuredIt(t *testing.T) {
	for _, predecessor := range []bool{false, true} {
		s := NewStore()
		if predecessor {
			pushVehicle(t, s, "audi-vag", .48, .8, "Stopped", false, 48*time.Hour)
		}
		soc := .98
		s.Update("audi-vag", DerVehicle, 0, &soc, json.RawMessage(`{"soc":98,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
		s.EmitMetric("audi-vag", "vehicle_soc_age_s", 156022, "s", "", "")
		s.DriverHealthMut("audi-vag").RecordSuccess()
		_, receivedAt, _ := s.LatestMetric("audi-vag", "vehicle_soc_age_s")
		o, ok := observation(t, s, "audi-vag")
		if !ok || o.SoC != .98 || !o.Stale || !o.At.Equal(receivedAt.Add(-156022*time.Second)) || o.Live(receivedAt) {
			t.Fatalf("predecessor=%v: cached report %+v", predecessor, o)
		}
	}
}

func TestACachedReportNeedsAValidAgeAndSoC(t *testing.T) {
	for _, report := range []string{`{"soc":98,"soc_fresh":false,"stale":true}`, `{"soc":101,"soc_fresh":false,"stale":true}`, `{"soc":98,"soc_fresh":"false","stale":true}`} {
		s := NewStore()
		soc := .98
		s.Update("audi", DerVehicle, 0, &soc, json.RawMessage(report))
		s.DriverHealthMut("audi").RecordSuccess()
		if got := VehicleObservations(s); len(got) != 0 {
			t.Fatalf("invalid report or unknown age: %+v", got)
		}
		if report != `{"soc":98,"soc_fresh":false,"stale":true}` {
			s.EmitMetric("audi", "vehicle_soc_age_s", 156022, "s", "", "")
			if got := VehicleObservations(s); len(got) != 0 {
				t.Fatalf("invalid raw report: %+v", got)
			}
		}
	}
}

// Before cars a reading older than an hour could not anchor a level. Now a
// level is the car's last reading however old, and the energy rule and the
// car's unplugs say whether it is still exact.
func TestAnHourOldReportIsStillTheCarsLastReading(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "cloud", 0.50, 0.80, "Stopped", false, 61*time.Minute)
	o, ok := observation(t, s, "cloud")
	if !ok || o.SoC != .50 || time.Since(o.At) < 61*time.Minute || o.Live(time.Now()) {
		t.Fatalf("hour-old report %+v", o)
	}
}

func TestADriverStaleReplayKeepsTheMeasurementTime(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "vag", .78, .80, "Stopped", false, 0)
	aged := time.Now().Add(-34 * time.Minute)
	s.mu.Lock()
	if r := s.readings[key("vag", DerVehicle)]; r != nil {
		r.SoCUpdatedAt = aged
	}
	s.mu.Unlock()
	cached := .78
	s.Update("vag", DerVehicle, 0, &cached, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
	s.EmitMetric("vag", "vehicle_soc_age_s", 34*60, "s", "", "")
	o, ok := observation(t, s, "vag")
	if !ok || o.SoC != .78 || !o.Stale || time.Since(o.At) < 33*time.Minute {
		t.Fatalf("replay of a 34-minute measurement: %+v", o)
	}
}

func TestACachedReportDoesNotReplaceANewerCarObservation(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "vag", .80, .80, "Stopped", false, time.Minute)
	latest := s.Get("vag", DerVehicle).SoCUpdatedAt
	soc := .78
	s.Update("vag", DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":false,"charging_state":"Stopped"}`))
	s.EmitMetric("vag", "vehicle_soc_age_s", 34*60, "s", "", "")
	o, ok := observation(t, s, "vag")
	if !ok || o.SoC != .80 || !o.At.Equal(latest) {
		t.Fatalf("older cached report replaced newer car measurement: %+v", o)
	}
}

// The car's own word on its cable and place reaches the cars runtime.
func TestVehicleObservationsCarryPluggedAndAtSite(t *testing.T) {
	s := NewStore()
	soc := .4
	s.Update("tesla-ha", DerVehicle, 0, &soc, json.RawMessage(`{"plugged":true,"at_site":false}`))
	s.DriverHealthMut("tesla-ha").RecordSuccess()
	o, ok := observation(t, s, "tesla-ha")
	if !ok || o.Plugged == nil || !*o.Plugged || o.AtSite == nil || *o.AtSite {
		t.Fatalf("plugged/at_site %+v", o)
	}
}
