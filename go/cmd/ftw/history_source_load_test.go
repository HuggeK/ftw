package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHistoryLoadIndependentReportingCadence(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("grid", telemetry.DerMeter, 4000, nil, nil)
	tel.RecordDriverSuccess("grid")
	tel.Update("charger", telemetry.DerEV, 3000, nil, nil)
	tel.RecordDriverSuccess("charger")
	// Both powers are fresh. The charger reports more slowly than the meter.
	tel.Get("charger", telemetry.DerEV).UpdatedAt = time.Now().Add(-45 * time.Second)
	base := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 30; i++ {
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, 90*time.Second)
		if p.Sources.Missing != 0 {
			t.Fatal("fresh charger rejected by historical source qualification")
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := st.DailyEnergy(base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-1000.0/12) > 0.001 {
		t.Fatal("fresh asynchronous sources lost historical household energy")
	}
	native, err := (&appHistory{st: st}).Window(context.Background(), []string{"load_w"}, 300000, base, base+300000)
	if err != nil {
		t.Fatal(err)
	}
	if len(native["load_w"]) != 1 || math.Abs(native["load_w"][0].W-1000) > 0.001 {
		t.Fatal("native history did not receive the interval household balance")
	}
}

// A silent inverter is unknown on its own. Its balance with the meter is
// held, not voided, so the household keeps its estimated energy.
func TestHistoryHoldsASilentSourceThroughTheBalance(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.Update("grid", telemetry.DerMeter, 1000, nil, nil)
	tel.RecordDriverSuccess("grid")
	tel.Update("sma", telemetry.DerPV, 0, nil, nil)
	tel.RecordDriverSuccess("sma")
	base := time.Now().Add(-20 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
	for i := int64(0); i <= 60; i++ {
		if i == 6 {
			tel.Get("sma", telemetry.DerPV).UpdatedAt = time.Now().Add(-10 * time.Minute)
		}
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "grid"}, base+i*10000, 90*time.Second)
		if i >= 6 && p.Sources.Missing != state.MissingPV {
			t.Fatalf("silent inverter marked %b, want only its own flow", p.Sources.Missing)
		}
		p.Sources.AtMS = base + i*10000
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := st.DailyEnergy(base, base+600000)
	if err != nil {
		t.Fatal(err)
	}
	if d.Missing&state.MissingLoad != 0 || math.Abs(d.LoadWh-1000.0/6) > 0.001 {
		t.Fatalf("household lost through a silent inverter: %v Wh, quality %s", d.LoadWh, d.Missing.Quality())
	}
}

func TestHistorySourceQualificationDoesNotInventMissingFlows(t *testing.T) {
	for _, tc := range []struct {
		reason string
		watts  float64
		known  bool
		want   state.HistoryMissing
	}{
		{"missing_config", 0, true, state.MissingLoad},
		{"duplicate_charger:easee", 0, true, state.MissingLoad},
		{"invalid_balance", 1000, true, 0},
		{"nonfinite_sum", math.Inf(1), false, state.MissingLoad | state.MissingEV},
		{"stale:easee:ev", 0, false, state.MissingEV},
		{"offline:easee:ev", 0, false, state.MissingEV},
	} {
		known := map[telemetry.DerType]bool{telemetry.DerMeter: true, telemetry.DerPV: true, telemetry.DerBattery: true, telemetry.DerEV: tc.known, telemetry.DerV2X: true}
		p := historySources(telemetry.ForecastReading{At: time.Now(), Known: known, Reason: tc.reason, EVW: tc.watts})
		if p.Missing != tc.want || math.IsNaN(p.EVW) || math.IsInf(p.EVW, 0) {
			t.Fatal("history source qualification lost missing evidence or rejected unrelated telemetry")
		}
	}
}
