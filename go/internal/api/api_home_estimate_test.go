package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestStatusHomeEstimateIsSeparateFromInstantaneousPower(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("site")
	base := time.Now().Add(-20 * time.Second)
	for i := 0; i <= 10; i++ {
		data, _ := json.Marshal(map[string]any{"power_observed_at": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)})
		tel.Update("site", telemetry.DerMeter, 1000+float64(i)*100, nil, data)
	}
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	for i := 0; i < 3; i++ {
		srv.qualifyStatusHistory(resp, "site")
		e, ok := resp["load_estimate"].(telemetry.HomeEstimate)
		if !ok || e.Watts == nil || math.Abs(*e.Watts-1500) > 1e-6 || e.AgeMS < 15000 || e.AgeMS > 20000 || e.Method != "aligned_balance_10s_v1" {
			t.Fatal(e)
		}
		if resp["load_w"] != float64(2000) || resp["grid_w"] != float64(2000) || resp["history_quality"] != "complete" {
			t.Fatal(resp)
		}
	}
	tel.DriverHealthMut("site").SetOffline()
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["load_estimate"].(telemetry.HomeEstimate).Watts != nil {
		t.Fatal(resp)
	}
}

func TestDailyEnergyCacheWaitsForMidnightRecovery(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	// Include the 23/25-hour days: a local day is not always 24 hours.
	for _, day := range []time.Time{time.Date(2026, 10, 17, 0, 0, 0, 0, loc), time.Date(2026, 10, 25, 0, 0, 0, 0, loc), time.Date(2026, 3, 29, 0, 0, 0, 0, loc)} {
		end := day.AddDate(0, 0, 1)
		for _, elapsed := range []time.Duration{-time.Second, 0, 30 * time.Second, 59 * time.Second} {
			if dailyEnergyCacheReady(day, end.Add(elapsed)) {
				t.Fatal("cached before recovery can finish", day, elapsed)
			}
		}
		if !dailyEnergyCacheReady(day, end.Add(time.Minute)) {
			t.Fatal("closed day never became cacheable", day)
		}
	}
}
