package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLiveMeasurementUsesOneDisplayPolicyWithoutChangingHistory(t *testing.T) {
	s := NewStore()
	for _, driver := range []string{"meter", "battery"} {
		s.EnsureDriverHealth(driver)
	}
	at := time.Now().Add(-3 * time.Second)
	data, err := json.Marshal(map[string]any{"control_power_w": -2000, "control_power_available": true, "control_power_observed_at": at.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal("invalid power evidence")
	}
	s.Update("meter", DerMeter, 1000, nil, nil)
	s.Update("battery", DerBattery, -2500, nil, data)
	s.DriverHealthMut("battery").WatchdogTimeoutOverride = 10 * time.Second
	opts := ForecastOptions{MaxAge: time.Second}
	live := s.LiveMeasurementNow("meter", opts)
	if !live.Valid || live.BatteryW != -2000 || live.HouseholdW != 3000 {
		t.Fatal("live policy did not use measured AC power and the driver cadence")
	}
	if opts.UseControlPower || opts.UseWatchdogTimeout {
		t.Fatal("live query mutated the caller's history policy")
	}
	raw := s.ForecastMeasurementNow("meter", ForecastOptions{})
	if !raw.Valid || raw.BatteryW != -2500 || raw.HouseholdW != 3500 {
		t.Fatal("live query changed raw forecast/history power")
	}
	s.DriverHealthMut("battery").WatchdogTimeoutOverride = 0
	if s.LiveMeasurementNow("meter", opts).Valid {
		t.Fatal("stale source clock ignored without its cadence override")
	}
}

func TestLiveMeasurementAllowsIndependentCurrentSourceTimes(t *testing.T) {
	for _, skew := range []time.Duration{time.Second, 30 * time.Second} {
		t.Run(skew.String(), func(t *testing.T) {
			s := NewStore()
			for _, driver := range []string{"meter", "solar", "other-solar"} {
				s.EnsureDriverHealth(driver)
			}
			s.Update("meter", DerMeter, 1000, nil, nil)
			s.Update("solar", DerPV, -500, nil, nil)
			data, err := json.Marshal(map[string]any{
				"control_power_observed_at": time.Now().Add(-45 * time.Second).Format(time.RFC3339Nano),
			})
			if err != nil {
				t.Fatal(err)
			}
			s.Update("other-solar", DerPV, -250, nil, data)
			opts := ForecastOptions{MaxAge: 90 * time.Second, MaxSkew: skew, UseControlPower: true}
			live := s.LiveMeasurementNow("meter", opts)
			if !live.Valid || !live.PVValid || live.HouseholdW != 1750 {
				t.Fatalf("current independent sources hid live load: valid=%t, pv_valid=%t, reason=%q", live.Valid, live.PVValid, live.Reason)
			}
			forecast := s.ForecastMeasurementNow("meter", opts)
			if forecast.Valid || forecast.PVValid || forecast.Reason != "time_skew" || forecast.PVReason != "pv_time_skew" {
				t.Fatal("live query relaxed the forecast's source alignment")
			}
			// Relaxing alignment does not turn an expired source into live data.
			opts.MaxAge = 40 * time.Second
			if s.LiveMeasurementNow("meter", opts).Valid {
				t.Fatal("an expired source was accepted as live")
			}
		})
	}
}
