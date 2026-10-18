package telemetry

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func homeDisconnectedData(base time.Time) map[string]any {
	return map[string]any{
		"connected": false, "charging": false,
		"control_power_observed_at": base.Add(-time.Hour).Format(time.RFC3339Nano),
		"control_power_available":   true, "control_power_confirmed": true,
		"state_observed_at": base.Add(-time.Hour).Format(time.RFC3339Nano),
	}
}

func homeIdleSample(t *testing.T, s *Store, w float64, fields map[string]any, at time.Time) {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal("cannot encode charger evidence")
	}
	homeDataSample(s, "charger", DerEV, w, data, at)
}

// Easee can retain the last session's energy and change-only power timestamps
// for hours after unplugging. Confirmed disconnected polls describe zero EV
// load, not fresh measurements of that old session.
func TestHomeEstimateDisconnectedChargerWithOldObservations(t *testing.T) {
	for _, oldEnergy := range []bool{false, true} {
		t.Run(map[bool]string{false: "power_only", true: "previous_session_energy"}[oldEnergy], func(t *testing.T) {
			s := NewStore()
			base := time.Unix(100000, 0)
			fields := homeDisconnectedData(base)
			if oldEnergy {
				fields["session_wh"] = 5000
				fields["energy_observed_at"] = base.Add(-time.Hour).Format(time.RFC3339Nano)
			}
			for sec := 0; sec <= 600; sec++ {
				at := base.Add(time.Duration(sec) * time.Second)
				homeReceiptSample(s, "site", DerMeter, -1500, at)
				homeReceiptSample(s, "pv", DerPV, -3000, at)
				homeReceiptSample(s, "battery", DerBattery, 500, at)
				if sec%15 == 0 {
					homeIdleSample(t, s, 0, fields, at)
				}
			}
			now := base.Add(600 * time.Second)
			got := s.HomeUseEstimate(now, "site", ForecastOptions{})
			if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.Reason != "" {
				t.Fatalf("disconnected charger hid household load: %+v", got)
			}
			if got.TimingBasis != "receipt" || got.ToMS != now.UnixMilli() || got.ToMS-got.FromMS != 15000 || got.EnergyBasis != "power_integral" {
				t.Fatalf("wrong interval or clock basis: %+v", got)
			}
			// Display estimates must leave raw power and control evidence alone.
			reading := s.readings[key("charger", DerEV)]
			p, ok := ControlPowerObservation(reading.RawW, reading.Data, now)
			if !ok || !p.At.Equal(base.Add(-time.Hour)) || !p.ConfirmedAt.Equal(now) || reading.RawW != 0 {
				t.Fatal("display estimate changed source/control evidence")
			}
			// Asking again cannot extend the interval without another poll.
			again := s.HomeUseEstimate(now.Add(time.Second), "site", ForecastOptions{})
			if again.ToMS != got.ToMS || again.EndAgeMS != 1000 {
				t.Fatal("client reads extended disconnected coverage")
			}
			if stale := s.HomeUseEstimate(now.Add(homeLookback+time.Second), "site", ForecastOptions{}); stale.Watts != nil {
				t.Fatal("old disconnected polls remained usable")
			}
		})
	}
}

func TestHomeEstimateDoesNotInferDisconnectedZeroFromOtherEvidence(t *testing.T) {
	for _, scenario := range []string{"connected", "charging", "missing_connected", "missing_charging", "unconfirmed", "unavailable", "missing_availability", "nonzero", "control_nonzero", "session", "missing_state_time", "bad_state_time", "future_state_time", "future_power_time", "offline"} {
		t.Run(scenario, func(t *testing.T) {
			s := NewStore()
			base := time.Unix(100000, 0)
			fields := homeDisconnectedData(base)
			w := 0.0
			switch scenario {
			case "connected":
				fields["connected"] = true
			case "charging":
				fields["charging"] = true
			case "missing_connected":
				delete(fields, "connected")
			case "missing_charging":
				delete(fields, "charging")
			case "unconfirmed":
				fields["control_power_confirmed"] = false
			case "unavailable":
				fields["control_power_available"] = false
			case "missing_availability":
				delete(fields, "control_power_available")
			case "nonzero":
				w = 500
			case "control_nonzero":
				fields["control_power_w"] = 500
			case "session":
				fields["session_id"] = "active-session"
			case "missing_state_time":
				delete(fields, "state_observed_at")
			case "bad_state_time":
				fields["state_observed_at"] = "invalid"
			case "future_state_time":
				fields["state_observed_at"] = base.Add(time.Hour).Format(time.RFC3339Nano)
			case "future_power_time":
				fields["control_power_observed_at"] = base.Add(time.Hour).Format(time.RFC3339Nano)
			}
			for sec := 0; sec <= 60; sec++ {
				at := base.Add(time.Duration(sec) * time.Second)
				homeReceiptSample(s, "site", DerMeter, 1000+w, at)
				if sec%15 == 0 {
					homeIdleSample(t, s, w, fields, at)
				}
			}
			if scenario == "offline" {
				s.health["charger"].SetOffline()
			}
			if got := s.HomeUseEstimate(base.Add(time.Minute), "site", ForecastOptions{}); got.Watts != nil {
				t.Fatalf("%s became a zero-load interval: %+v", scenario, got)
			}
		})
	}
}

func TestHomeDisconnectedCoverageBreaksAtTransitions(t *testing.T) {
	for _, scenario := range []string{"reconnect", "unknown", "state_change", "offline", "gap", "remove"} {
		t.Run(scenario, func(t *testing.T) {
			s := NewStore()
			base := time.Unix(100000, 0)
			fields := homeDisconnectedData(base)
			for sec := 0; sec <= 60; sec++ {
				at := base.Add(time.Duration(sec) * time.Second)
				homeReceiptSample(s, "site", DerMeter, 1000, at)
				if sec%15 == 0 {
					homeIdleSample(t, s, 0, fields, at)
				}
			}
			if got := s.HomeUseEstimate(base.Add(time.Minute), "site", ForecastOptions{}); got.Watts == nil {
				t.Fatal("missing initial disconnected interval")
			}
			now := base.Add(61 * time.Second)
			switch scenario {
			case "reconnect":
				fields["connected"] = true
			case "unknown":
				fields["control_power_available"] = false
			case "state_change":
				fields["state_observed_at"] = now.Format(time.RFC3339Nano)
			case "offline":
				s.health["charger"].SetOffline()
			case "gap":
				now = base.Add(91 * time.Second)
			case "remove":
				s.Remove("charger")
			}
			homeReceiptSample(s, "site", DerMeter, 1000, now)
			homeIdleSample(t, s, 0, fields, now)
			if got := s.HomeUseEstimate(now, "site", ForecastOptions{}); got.Watts != nil {
				t.Fatalf("crossed %s: %+v", scenario, got)
			}
			// A newly bracketed disconnected interval may become usable again.
			for sec := 1; sec <= 30; sec++ {
				at := now.Add(time.Duration(sec) * time.Second)
				homeReceiptSample(s, "site", DerMeter, 1000, at)
				if sec%15 == 0 {
					homeIdleSample(t, s, 0, homeDisconnectedData(base), at)
				}
			}
			got := s.HomeUseEstimate(now.Add(30*time.Second), "site", ForecastOptions{})
			if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.FromMS < now.UnixMilli() {
				t.Fatalf("new interval did not recover after %s: %+v", scenario, got)
			}
		})
	}
}

func TestHomeEstimateKeepsChargingCountersSeparateFromDisconnectedCoverage(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 180; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		grid := 7000.0
		if sec > 60 && sec <= 90 {
			grid = 1000
		}
		homeReceiptSample(s, "site", DerMeter, grid, at)
		if sec%15 != 0 {
			continue
		}
		fields := homeDisconnectedData(base)
		w := 6000.0
		fields["session_wh"] = float64(sec) * w / 3600
		fields["energy_observed_at"] = at.Format(time.RFC3339Nano)
		switch {
		case sec <= 60:
			fields["connected"], fields["charging"], fields["session_id"] = true, true, "first-session"
		case sec <= 90:
			w = 0
			fields["state_observed_at"] = base.Add(61 * time.Second).Format(time.RFC3339Nano)
			fields["energy_observed_at"] = base.Add(time.Minute).Format(time.RFC3339Nano)
			fields["session_wh"] = 100
		default:
			fields["connected"], fields["charging"], fields["session_id"] = true, true, "second-session"
			fields["session_wh"] = float64(sec-90) * w / 3600
		}
		homeIdleSample(t, s, w, fields, at)
		got := s.HomeUseEstimate(at, "site", ForecastOptions{})
		switch sec {
		case 60, 90, 180:
			if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 {
				t.Fatalf("lost a supported interval at %d: %+v", sec, got)
			}
			if sec != 90 && got.EnergyBasis != "mixed" {
				t.Fatal("charging no longer uses session energy")
			}
		case 75, 105:
			if got.Watts != nil {
				t.Fatal("estimate crossed a plug/session transition")
			}
		}
	}
}
