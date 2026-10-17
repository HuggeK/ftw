package telemetry

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func pendingBatteryOrder(s *Store, watts float64, at time.Time, result string) {
	payload := []byte(fmt.Sprintf(`{"action":"battery","power_w":%g}`, watts))
	c := s.BeginCommand("battery", payload, at)
	s.CompleteCommand(c, result)
}

func pendingBatterySample(s *Store, rawW float64, sourceAt, receivedAt time.Time, extra map[string]any) {
	d := map[string]any{"control_power_observed_at": sourceAt.Format(time.RFC3339Nano)}
	for k, v := range extra {
		d[k] = v
	}
	data, _ := json.Marshal(d)
	s.mu.Lock()
	s.observeCommand("battery", DerBattery, rawW, data, receivedAt)
	s.mu.Unlock()
}

func requireBatteryUpper(t *testing.T, s *Store, want float64) {
	t.Helper()
	if got, known := s.BatteryCommandUpperW("battery"); !known || got != want {
		t.Fatalf("battery order upper=%v known=%v; want %v", got, known, want)
	}
}

func TestBatteryCommandUpperRetainsUnconfirmedOrder(t *testing.T) {
	for _, result := range []string{"pending", "accepted", "failed", "unknown"} {
		t.Run(result, func(t *testing.T) {
			s, start := NewStore(), time.Now()
			pendingBatteryOrder(s, 8000, start, result)
			// More than Recent's eight entries, all lower and all successful.
			for i := 1; i <= 12; i++ {
				pendingBatteryOrder(s, 0, start.Add(time.Duration(i)*time.Second), "accepted")
			}
			requireBatteryUpper(t, s, 8000)
			// Receipt time and a setpoint echo cannot replace source time.
			pendingBatterySample(s, 0, start.Add(2*time.Second), start.Add(20*time.Second), map[string]any{"setpoint_w": 0})
			requireBatteryUpper(t, s, 8000)
			pendingBatterySample(s, 0, start.Add(15*time.Second), start.Add(20*time.Second), nil)
			requireBatteryUpper(t, s, 8000)
			// AC power, rather than a different DC power, proves the lower run.
			pendingBatterySample(s, 2500, start.Add(16*time.Second), start.Add(20*time.Second), map[string]any{"control_power_w": 0})
			requireBatteryUpper(t, s, 0)
		})
	}
}

func TestBatteryCommandUpperRetainsIntermediateOrder(t *testing.T) {
	s, start := NewStore(), time.Now()
	pendingBatteryOrder(s, 8000, start, "accepted")
	pendingBatteryOrder(s, 3000, start.Add(time.Second), "failed")
	for i := 2; i <= 12; i++ {
		pendingBatteryOrder(s, 0, start.Add(time.Duration(i)*time.Second), "accepted")
	}
	// The 3 kW order may still arrive even though it left Recent.
	pendingBatterySample(s, 0, start.Add(16*time.Second), start.Add(16*time.Second), nil)
	requireBatteryUpper(t, s, 3000)
	for i := 17; i <= 32; i++ {
		pendingBatteryOrder(s, 0, start.Add(time.Duration(i)*time.Second), "accepted")
	}
	pendingBatterySample(s, 0, start.Add(32*time.Second), start.Add(32*time.Second), nil)
	requireBatteryUpper(t, s, 0)
}

func TestBatteryCommandUpperWaitsForTheLowerRun(t *testing.T) {
	s, start := NewStore(), time.Now()
	pendingBatteryOrder(s, 8000, start.Add(-time.Minute), "accepted")
	pendingBatteryOrder(s, 0, start, "accepted")
	// The high order is old, but this source sample predates the new lower run.
	pendingBatterySample(s, 0, start.Add(-time.Second), start.Add(20*time.Second), nil)
	requireBatteryUpper(t, s, 8000)
	for _, d := range []map[string]any{
		{"control_power_available": false},
		{"control_power_observed_at": "bad"},
		{"control_power_observed_at": start.Add(time.Hour).Format(time.RFC3339Nano)},
	} {
		pendingBatterySample(s, 0, start.Add(20*time.Second), start.Add(20*time.Second), d)
		requireBatteryUpper(t, s, 8000)
	}
	pendingBatterySample(s, 0, start.Add(20*time.Second), start.Add(20*time.Second), nil)
	requireBatteryUpper(t, s, 0)
}

func TestBatteryCommandUpperSurvivesDefaultAndReset(t *testing.T) {
	for _, mode := range []string{"default", "failed default", "failed order then default", "unknown order then default", "clear", "remove"} {
		t.Run(mode, func(t *testing.T) {
			s, start := NewStore(), time.Now()
			pendingBatteryOrder(s, 8000, start.Add(-time.Minute), "accepted")
			if mode == "failed order then default" {
				pendingBatteryOrder(s, 8000, start.Add(-time.Second), "failed")
			} else if mode == "unknown order then default" {
				pendingBatteryOrder(s, 8000, start.Add(-time.Second), "unknown")
			}
			switch mode {
			case "default", "failed default", "failed order then default", "unknown order then default":
				s.endCommandControlAt("battery", mode == "failed default", start)
			case "clear":
				s.ClearCommandEvidence("battery")
			case "remove":
				s.Remove("battery")
			}
			requireBatteryUpper(t, s, 8000)
			if mode == "clear" || mode == "remove" {
				if _, visible := s.CommandEvidence("battery", "battery"); visible {
					t.Fatal("reset retained response/UI evidence")
				}
			}
			pendingBatterySample(s, 0, start.Add(10*time.Second), start.Add(20*time.Second), nil)
			requireBatteryUpper(t, s, 8000)
			pendingBatterySample(s, 4000, start.Add(20*time.Second), start.Add(20*time.Second), nil)
			requireBatteryUpper(t, s, 4000)
			pendingBatterySample(s, 0, start.Add(40*time.Second), start.Add(40*time.Second), nil)
			requireBatteryUpper(t, s, 0)
		})
	}
}

func TestBatteryCommandUpperRepeatedDefaultDoesNotRestartWait(t *testing.T) {
	for _, failed := range []bool{false, true} {
		s, start := NewStore(), time.Now()
		pendingBatteryOrder(s, 8000, start, "accepted")
		for i := 1; i <= 12; i++ {
			at := start.Add(time.Duration(i) * 5 * time.Second)
			s.endCommandControlAt("battery", failed, at)
			pendingBatterySample(s, 0, at, at, nil)
		}
		requireBatteryUpper(t, s, 0)
	}
}

func TestBatteryDefaultCanStopDischarge(t *testing.T) {
	s, start := NewStore(), time.Now()
	pendingBatteryOrder(s, -5000, start, "accepted")
	s.endCommandControlAt("battery", true, start.Add(time.Second))
	requireBatteryUpper(t, s, 0)
}

func TestBatteryCommandUpperIncludesQueuedOfferAfterItsLastOrder(t *testing.T) {
	s, start := NewStore(), time.Now()
	pendingBatteryOrder(s, 0, start, "accepted")
	offer := CommandPoint{PowerW: 8000, At: start.Add(time.Second)}
	if upper, _ := s.BatteryCommandUpperW("battery", offer); upper != 8000 {
		t.Fatalf("computed offer waiting for driver actor lost: %.0fW", upper)
	}
	s.endCommandControlAt("battery", false, start.Add(2*time.Second))
	if upper, _ := s.BatteryCommandUpperW("battery", offer); upper != 0 {
		t.Fatalf("default retained an older computed offer: %.0fW", upper)
	}
	offer.At = start.Add(3 * time.Second)
	if upper, _ := s.BatteryCommandUpperW("battery", offer); upper != 8000 {
		t.Fatalf("new computed offer after default lost: %.0fW", upper)
	}
}
