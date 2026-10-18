package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHistoryDisabledBatteryKeepsP1Load(t *testing.T) {
	for _, knownCatalog := range []bool{true, false} {
		st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.EnableHistoryAggregation(); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Drivers: []config.Driver{
			{Name: "p1", Lua: "drivers/hybrid.lua", IsSiteMeter: true, BatteryCapacityWh: 10000, BatteryTelemetryOnly: true, Config: map[string]any{"read_pv": false, "read_battery": false}},
			{Name: "solar", Lua: "drivers/solar.lua"},
		}}
		catalog := measurementCatalog()
		if !knownCatalog {
			catalog = nil
		}
		tel := telemetry.NewStore()
		tel.Update("p1", telemetry.DerMeter, 1000, nil, nil)
		tel.RecordDriverSuccess("p1")
		tel.Update("solar", telemetry.DerPV, -2000, nil, nil)
		tel.RecordDriverSuccess("solar")
		opts := historyMeasurementOptions(cfg, catalog)
		at := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute).UnixMilli()
		for i := int64(0); i <= 30; i++ {
			for _, kind := range []telemetry.DerType{telemetry.DerMeter, telemetry.DerPV} {
				name := map[telemetry.DerType]string{telemetry.DerMeter: "p1", telemetry.DerPV: "solar"}[kind]
				tel.Get(name, kind).UpdatedAt = time.Now()
			}
			p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "p1"}, at+i*10000, time.Minute, opts)
			p.Sources.AtMS = at + i*10000
			if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.FlushHistory(context.Background()); err != nil {
			t.Fatal(err)
		}
		points, err := st.LoadHistory(at, at+300000, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) == 0 {
			t.Fatal("history empty")
		}
		for _, p := range points {
			if p.Missing&(state.MissingBattery|state.MissingLoad) != 0 || p.LoadW != 3000 {
				t.Fatalf("catalog=%v: disabled battery blocked saved P1 load: %+v", knownCatalog, points)
			}
		}
		native, err := (&appHistory{st: st}).Window(context.Background(), []string{"load_w"}, state.EnergyLedgerBucketMS, at/state.EnergyLedgerBucketMS*state.EnergyLedgerBucketMS, at+state.EnergyLedgerBucketMS)
		if err != nil {
			t.Fatal(err)
		}
		if len(native["load_w"]) == 0 || native["load_w"][0].W <= 0 {
			t.Fatal("saved household ledger did not reach native app history")
		}
		// A previously observed battery remains significant even if config no
		// longer requires its first reading. Never replace a stale flow by zero.
		tel.Update("p1", telemetry.DerBattery, 500, nil, nil)
		tel.Get("p1", telemetry.DerBattery).UpdatedAt = time.Now().Add(-2 * time.Minute)
		p, _ := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "p1"}, at, time.Minute, opts)
		if p.Missing&(state.MissingBattery|state.MissingLoad) != state.MissingBattery|state.MissingLoad {
			t.Fatal("disabled config hid an observed stale battery")
		}
	}
}

func TestBatteryReadingConfigDeclaresRequiredFlow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		read     any
		capacity float64
		want     bool
	}{
		{"disabled despite stored capacity", false, 10000, false},
		{"existing battery", nil, 10000, true},
		{"explicit telemetry reading", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := config.Driver{Name: "hybrid", Lua: "drivers/hybrid.lua", BatteryCapacityWh: tc.capacity}
			if tc.read != nil {
				d.Config = map[string]any{"read_battery": tc.read, "read_pv": false}
			}
			opts := forecastMeasurementOptions(&config.Config{Drivers: []config.Driver{d}}, measurementCatalog())
			found := false
			for _, f := range opts.ExpectedFlows {
				found = found || f.DerType == telemetry.DerBattery
			}
			if found != tc.want {
				t.Fatalf("battery required=%v, want %v", found, tc.want)
			}
		})
	}
}

func TestPVZeroCounterKeepsObservedEnergy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		counters []float64
		watts    float64
		step     time.Duration
		wantWh   float64
	}{
		{"unavailable zero", []float64{0, 0, 0}, 3000, time.Minute, 100},
		{"positive counter returns", []float64{0, 0, 100, 150}, 3000, time.Minute, 150},
		{"previous Core zero cursor", []float64{0, 0, 100, 150}, 3000, time.Minute, 150},
		{"counter resets to zero", []float64{100, 0, 0, 50}, 3000, time.Minute, 150},
		{"real zero generation", []float64{0, 0, 0}, 0, time.Minute, 0},
		{"positive coarse counter stays authoritative", []float64{100, 100, 100}, 3000, time.Minute, 0},
		{"unobserved gap stays missing", []float64{0, 0}, 3000, 5 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			st, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			id, err := st.RegisterDevice(state.Device{DriverName: "solar", Make: "PV meter", Serial: "test-pv"})
			if err != nil {
				t.Fatal(err)
			}
			tel := telemetry.NewStore()
			base := time.Now().Add(-time.Hour).Truncate(5 * time.Minute).UnixMilli()
			for i, counter := range tc.counters {
				at := base + int64(i)*tc.step.Milliseconds()
				raw, _ := json.Marshal(map[string]float64{"generation_wh": counter})
				tel.Update("solar", telemetry.DerPV, -tc.watts, nil, raw)
				tel.RecordDriverSuccess("solar")
				tel.Get("solar", telemetry.DerPV).UpdatedAt = time.UnixMilli(at)
				obs := buildEnergyObservations(st, tel, tickPersistControl{}, state.HistoryPoint{}, testEnergyIdentity(st))
				if i == 0 && tc.name == "previous Core zero cursor" {
					// Seed exactly the zero-counter cursor the older Core kept.
					obs[0].CounterWh = &counter
				}
				if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: at, JSON: "{}"}, nil, obs); err != nil {
					t.Fatal(err)
				}
				if i == 1 && (tc.name == "positive counter returns" || tc.name == "previous Core zero cursor") {
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
					st, err = state.Open(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			points, _, err := st.LoadEnergyHistory(state.EnergyHistoryQuery{AssetID: state.HardwareEnergyAssetID(id, state.AssetPV), SinceMS: base, UntilMS: base + int64(len(tc.counters))*tc.step.Milliseconds(), BucketMS: state.EnergyLedgerBucketMS, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var wh float64
			for _, p := range points {
				wh += p.EnergyWh
				if tc.name == "unavailable zero" && p.EnergyWh > 0 && (p.Source != "power_telemetry" || p.Quality != "integrated") {
					t.Fatal("power-derived energy claimed a measured counter")
				}
			}
			if math.Abs(wh-tc.wantWh) > 0.0001 {
				t.Fatalf("saved PV = %g Wh, want %g Wh", wh, tc.wantWh)
			}
		})
	}
}
