package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A hybrid without a declared PV reading: History leaves its PV out of Home
// use, the forecast requires it, and the forecast revision must not change.
func optionalPVConfig() *config.Config {
	return &config.Config{Drivers: []config.Driver{
		{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true},
		{Name: "hybrid", Lua: "drivers/hybrid.lua", BatteryCapacityWh: 10000},
	}}
}

func TestHistoryOptionsKeepOptionalPVForTraining(t *testing.T) {
	cfg := optionalPVConfig()
	history := historyMeasurementOptions(cfg, measurementCatalog())
	pv := telemetry.ForecastFlow{Driver: "hybrid", DerType: telemetry.DerPV}
	for _, f := range history.ExpectedFlows {
		if f == pv {
			t.Fatal("History requires optional PV")
		}
	}
	if len(history.OptionalFlows) != 1 || history.OptionalFlows[0] != pv {
		t.Fatalf("optional flows = %+v", history.OptionalFlows)
	}
	raw, err := json.Marshal(forecastMeasurementOptions(cfg, measurementCatalog()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "OptionalFlows") {
		t.Fatalf("forecast options changed their stored identity: %s", raw)
	}
}

func TestOptionalPVUnreadKeepsDaytimeOutOfTraining(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	pinned := historySun{Lat: 59.6, Lon: 16.5, Placed: true}
	flows := historyMeasurementOptions(optionalPVConfig(), measurementCatalog()).OptionalFlows
	for _, c := range []struct {
		name      string
		at        time.Time
		sun       historySun
		pvLive    bool
		estimated bool
	}{
		{"day, PV never reported", time.Date(2026, 10, 19, 12, 0, 0, 0, stockholm), pinned, false, true},
		{"twilight, PV never reported", time.Date(2026, 10, 19, 17, 45, 0, 0, stockholm), pinned, false, true},
		{"past civil dusk, PV never reported", time.Date(2026, 10, 19, 2, 22, 0, 0, stockholm), pinned, false, false},
		{"night without a pin", time.Date(2026, 10, 19, 2, 22, 0, 0, stockholm), historySun{}, false, true},
		{"day, PV reporting", time.Date(2026, 10, 19, 12, 0, 0, 0, stockholm), pinned, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			tel.Update("site", telemetry.DerMeter, 1000, nil, nil)
			tel.RecordDriverSuccess("site")
			if c.pvLive {
				tel.Update("hybrid", telemetry.DerPV, -2000, nil, nil)
				tel.RecordDriverSuccess("hybrid")
			}
			p := &state.HistorySources{AtMS: c.at.UnixMilli(), GridW: 1000}
			got := optionalPVUnread(p, tel, flows, c.sun)
			if got != c.estimated || (p.Missing&state.EstimatedLoad != 0) != c.estimated {
				t.Fatalf("estimated=%v missing=%b, want estimated=%v", got, p.Missing, c.estimated)
			}
			if p.Missing&state.MissingPower != 0 {
				t.Fatal("optional PV changed what History shows")
			}
		})
	}
}
