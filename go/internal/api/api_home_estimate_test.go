package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
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
		if !ok || e.Watts == nil || math.Abs(*e.Watts-1500) > 1e-6 || e.AgeMS < 15000 || e.AgeMS > 20000 || e.Method != "reported_energy_balance_v3" || e.TimingBasis != "source" || e.EnergyBasis != "power_integral" || e.EnergyWh == nil || e.EndAgeMS < 10000 {
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

func TestStatusKeepsPowerBalanceWhenSessionEnergyCannotFormAnInterval(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site", "charger"} {
		tel.EnsureDriverHealth(name)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	data, err := json.Marshal(map[string]any{
		"connected": true, "charging": false,
		"control_power_observed_at": old, "control_power_confirmed": true,
		"control_power_available": true,
		"session_wh":              5000, "energy_observed_at": old,
	})
	if err != nil {
		t.Fatal(err)
	}
	tel.Update("site", telemetry.DerMeter, 1234, nil, nil)
	tel.Update("charger", telemetry.DerEV, 0, nil, data)
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(1234) || resp["history_quality"] != "complete" {
		t.Fatal("valid power balance disappeared")
	}
	if e := resp["load_estimate"].(telemetry.HomeEstimate); e.Watts != nil || e.Reason != "incomplete_source_window" {
		t.Fatal("old session was presented as an energy interval")
	}
	tel.DriverHealthMut("charger").SetOffline()
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["load_estimate"].(telemetry.HomeEstimate).Watts != nil {
		t.Fatal("offline charger became zero load")
	}
	tel.RecordDriverSuccess("charger")
	tel.Update("charger", telemetry.DerEV, 6000, nil, nil)
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["history_reason"] != "invalid_balance" {
		t.Fatal("negative residual became a zero household balance")
	}
}

func TestStatusHomePowerUsesPixiiACWithoutChangingRawHistoryInputs(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"site", "battery", "charger"} {
		tel.EnsureDriverHealth(name)
	}
	tel.Update("site", telemetry.DerMeter, 5000, nil, nil)
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_w":-2000,"control_power_available":true}`))
	tel.Update("charger", telemetry.DerEV, 6000, nil, nil)
	srv := New(&Deps{Tel: tel, HistoryMeasurementSources: func() telemetry.ForecastOptions { return telemetry.ForecastOptions{} }})
	resp := map[string]any{}
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != float64(1000) || resp["bat_w"] != float64(-2000) {
		t.Fatal("household AC balance used battery DC power")
	}
	raw := tel.ForecastMeasurementNow("site", telemetry.ForecastOptions{})
	if raw.BatteryW != -2500 || raw.HouseholdW != 1500 {
		t.Fatal("display changed raw forecast/history input")
	}
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_available":false}`))
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["bat_w"] != nil {
		t.Fatal("missing AC measurement became DC power or zero")
	}
	tel.Update("battery", telemetry.DerBattery, -2500, nil, json.RawMessage(`{"control_power_w":-500,"control_power_available":true}`))
	srv.qualifyStatusHistory(resp, "site")
	if resp["load_w"] != nil || resp["history_reason"] != "invalid_balance" {
		t.Fatal("negative AC balance became zero or a DC estimate")
	}
}

func TestEnergyTodayKeepsEstimatedLoadOutOfMeasuredFields(t *testing.T) {
	got := energyTodayFields(state.DayEnergy{Intervals: 2, Missing: state.EstimatedLoad, LoadWh: 22, ImportWh: 10})
	if got["history_quality"] != "estimated" || got["load_wh"] != float64(22) {
		t.Fatal(got)
	}
	measured := got["measured"].(map[string]any)
	if measured["load_wh"] != nil || measured["import_wh"] != float64(10) {
		t.Fatal(measured)
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
		for _, elapsed := range []time.Duration{-time.Second, 0, 30 * time.Second, 59 * time.Second, 2 * time.Minute, 179 * time.Second} {
			if dailyEnergyCacheReady(day, end.Add(elapsed)) {
				t.Fatal("cached before recovery can finish", day, elapsed)
			}
		}
		if !dailyEnergyCacheReady(day, end.Add(3*time.Minute)) {
			t.Fatal("closed day never became cacheable", day)
		}
	}
}
