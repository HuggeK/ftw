package telemetry

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func homeSessionSample(s *Store, wh float64, session string, measured, received time.Time) {
	data, _ := json.Marshal(map[string]any{
		"session_wh": wh, "session_id": session, "energy_observed_at": measured.Format(time.RFC3339Nano),
		// Simulate a change-only power report that has not moved for an hour.
		"control_power_observed_at": received.Add(-time.Hour).Format(time.RFC3339Nano),
		"control_power_confirmed":   true,
	})
	homeDataSample(s, "cloud", DerEV, 9714, data, received)
}

func homeDataSample(s *Store, driver string, kind DerType, w float64, data json.RawMessage, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordHomeObservation(driver, kind, w, data, at)
	s.readings[key(driver, kind)] = &DerReading{Driver: driver, DerType: kind, RawW: w, Data: data, UpdatedAt: at}
	s.health[driver] = &DriverHealth{Name: driver}
}

func TestHomeEstimatePrefersSessionEnergyWithMinuteDeliveryDelay(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 180; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		// A triangular charge ramp: exactly 50 Wh in each full minute.
		phase := sec % 60
		ev := float64(phase) * 200
		if phase > 30 {
			ev = float64(60-phase) * 200
		}
		homeReceiptSample(s, "site", DerMeter, 1000+ev, at)
		if sec >= 60 && sec%60 == 0 {
			homeSessionSample(s, float64(sec/60-1)*50, "session-a", at.Add(-time.Minute), at)
		}
		if sec < 120 {
			continue
		}
		got := s.HomeUseEstimate(at, "site", ForecastOptions{})
		if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.EnergyWh == nil || math.Abs(*got.EnergyWh-1000.0/60) > 1e-8 || got.EnergyBasis != "mixed" {
			t.Fatalf("second %d: energy balance failed: %+v", sec, got)
		}
		if got.ToMS-got.FromMS != 60000 || got.EndAgeMS < 60000 || got.MinW != nil || got.MaxW != nil {
			t.Fatalf("counter energy presented as instantaneous power/range: %+v", got)
		}
	}
	// Display energy must not repair the bad raw power or change control evidence.
	raw := s.ForecastMeasurement(base.Add(180*time.Second), "site", ForecastOptions{})
	if raw.Valid || raw.Reason != "invalid_balance" || raw.HouseholdW != -8714 {
		t.Fatal(raw)
	}
}

func TestHomeEstimateAdaptsToCounterReportsAndDoesNotRefreshOnPoll(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 250; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		stamp := 0
		switch {
		case sec >= 180:
			stamp = 180
		case sec >= 60:
			stamp = 60
		}
		homeSessionSample(s, float64(stamp)*6000/3600, "session-a", base.Add(time.Duration(stamp)*time.Second), at)
		if sec != 60 && sec != 180 && sec != 250 {
			continue
		}
		got := s.HomeUseEstimate(at, "site", ForecastOptions{})
		period := int64(60000)
		if sec >= 180 {
			period = 120000
		}
		if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.ToMS-got.FromMS != period || got.EndAgeMS != int64(sec-stamp)*1000 {
			t.Fatal(sec, got)
		}
	}
}

func TestHomeSessionCounterBreaksAtInvalidEvidence(t *testing.T) {
	base := time.Unix(100000, 0)
	for _, scenario := range []string{"session", "reset", "missing_session", "missing_time", "missing_counter", "backward", "conflict", "future", "long_gap", "offline"} {
		t.Run(scenario, func(t *testing.T) {
			s := NewStore()
			for sec := 0; sec <= 60; sec++ {
				at := base.Add(time.Duration(sec) * time.Second)
				homeReceiptSample(s, "site", DerMeter, 7000, at)
				if sec%60 == 0 {
					homeSessionSample(s, float64(sec)*6000/3600, "session-a", at, at)
				}
			}
			now := base.Add(61 * time.Second)
			d := map[string]any{"session_wh": 101, "session_id": "session-a", "energy_observed_at": now.Format(time.RFC3339Nano)}
			switch scenario {
			case "session":
				d["session_id"] = "session-b"
			case "reset":
				d["session_wh"] = 0
			case "missing_session":
				delete(d, "session_id")
			case "missing_time":
				delete(d, "energy_observed_at")
			case "missing_counter":
				delete(d, "session_wh")
			case "backward":
				d["energy_observed_at"] = base.Add(59 * time.Second).Format(time.RFC3339Nano)
			case "conflict":
				d["energy_observed_at"] = base.Add(60 * time.Second).Format(time.RFC3339Nano)
			case "future":
				d["energy_observed_at"] = now.Add(time.Second).Format(time.RFC3339Nano)
			case "long_gap":
				now = base.Add(361 * time.Second)
				d["energy_observed_at"] = now.Format(time.RFC3339Nano)
			case "offline":
				s.health["cloud"].SetOffline()
			}
			data, _ := json.Marshal(d)
			homeDataSample(s, "cloud", DerEV, 6000, data, now)
			homeReceiptSample(s, "site", DerMeter, 7000, now)
			if got := s.HomeUseEstimate(now, "site", ForecastOptions{}); got.Watts != nil {
				t.Fatalf("crossed %s: %+v", scenario, got)
			}
		})
	}
}

func TestHomeEnergyCannotFillLocalPowerGap(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 120; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		if sec < 30 || sec > 90 {
			homeReceiptSample(s, "site", DerMeter, 7000, at)
		}
		if sec%60 == 0 {
			homeSessionSample(s, float64(sec)*6000/3600, "session-a", at, at)
		}
	}
	if got := s.HomeUseEstimate(base.Add(120*time.Second), "site", ForecastOptions{}); got.Watts != nil {
		t.Fatal("counter filled missing meter energy", got)
	}
}

func TestHomePowerPeriodFollowsDistinctSourceReports(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 60; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		if sec%20 == 0 {
			homeSample(s, "cloud", DerEV, 6000, at, at, false)
		}
	}
	got := s.HomeUseEstimate(base.Add(time.Minute), "site", ForecastOptions{})
	if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.ToMS-got.FromMS != 20000 || got.EnergyBasis != "power_integral" {
		t.Fatal(got)
	}
}

func TestHomeEnergyKeepsWholeReferenceCounterInterval(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 90; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		if sec == 0 || sec == 60 || sec == 90 {
			homeSessionSample(s, float64(sec)*6000/3600, "session-a", at, at)
		}
	}
	got := s.HomeUseEstimate(base.Add(90*time.Second), "site", ForecastOptions{})
	if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.FromMS != base.UnixMilli() || got.ToMS-got.FromMS != 90000 {
		t.Fatal("split the reference counter interval after a cadence change", got)
	}
}

func TestHomeEnergyUsesCountersWithoutPowerTimeAndDoesNotMixDCEnergy(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 60; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 5000, at)
		// Pixii: raw DC and cabinet counters do not describe the AC site flow.
		homeDataSample(s, "battery", DerBattery, -2500, json.RawMessage(`{"control_power_w":-2000,"charge_wh":0,"discharge_wh":9000}`), at)
		if sec%60 == 0 {
			data, _ := json.Marshal(map[string]any{"session_wh": float64(sec) * 6000 / 3600, "session_id": "session-a", "energy_observed_at": at.Format(time.RFC3339Nano)})
			homeDataSample(s, "cloud", DerEV, 12345, data, at)
		}
	}
	got := s.HomeUseEstimate(base.Add(time.Minute), "site", ForecastOptions{})
	if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 {
		t.Fatal("counter path required power time or used DC energy", got)
	}
}

func TestHomeEnergyBalancesTwoCounterSchedules(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 135; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 11000, at)
		for name, interval := range map[string]int{"charger-a": 60, "charger-b": 45} {
			if sec%interval != 0 {
				continue
			}
			w := 6000.0
			if name == "charger-b" {
				w = 4000
			}
			data, _ := json.Marshal(map[string]any{"session_wh": float64(sec) * w / 3600, "session_id": name, "energy_observed_at": at.Format(time.RFC3339Nano)})
			homeDataSample(s, name, DerEV, w, data, at)
		}
	}
	for i := 0; i < 20; i++ {
		got := s.HomeUseEstimate(base.Add(135*time.Second), "site", ForecastOptions{})
		if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.ToMS != base.Add(120*time.Second).UnixMilli() || got.FromMS != base.Add(time.Minute).UnixMilli() {
			t.Fatal("counter schedules did not share a stable period", got)
		}
	}
}

func TestHomeEstimateDropsSessionEnergyWhenDriverIsReplaced(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 60; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		if sec%60 == 0 {
			homeSessionSample(s, float64(sec)*6000/3600, "session-a", at, at)
		}
	}
	s.Remove("cloud")
	// The replacement has source power but does not expose session energy.
	for sec := 61; sec <= 81; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		homeSample(s, "cloud", DerEV, 6000, at, at, false)
	}
	got := s.HomeUseEstimate(base.Add(81*time.Second), "site", ForecastOptions{})
	if got.Watts == nil || math.Abs(*got.Watts-1000) > 1e-8 || got.EnergyBasis != "power_integral" {
		t.Fatal("replacement inherited the old counter requirement", got)
	}
}

func TestHomeEnergyRepeatedPollsCannotKeepAnOldIntervalFresh(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 241; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 7000, at)
		stamp := 0
		if sec >= 60 {
			stamp = 60
		}
		homeSessionSample(s, float64(stamp)*6000/3600, "session-a", base.Add(time.Duration(stamp)*time.Second), at)
		if sec < 240 {
			continue
		}
		got := s.HomeUseEstimate(at, "site", ForecastOptions{})
		if sec == 240 && (got.Watts == nil || got.EndAgeMS != 180000) {
			t.Fatal("lost supported past interval", got)
		}
		if sec == 241 && got.Watts != nil {
			t.Fatal("fresh HTTP reply extended energy age", got)
		}
	}
}
