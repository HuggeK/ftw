package api

import (
	"context"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCloudStatisticsConfiguredLimitsReadOnlyUnknownAndPartner(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "bat1", BatteryCapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 5000, InverterGroup: "shared"},
		{Name: "bat2", BatteryCapacityWh: 5000, MaxChargeW: 4000, InverterGroup: "shared"},
		{Name: "observed", BatteryCapacityWh: 20000, MaxChargeW: 10000, ObserveOnly: true},
		{Name: "disabled", BatteryCapacityWh: 99999, Disabled: true}, {Name: "ev"},
	}, Loadpoints: []config.Loadpoint{{DriverName: "ev", MaxChargeW: 11000}, {DriverName: "ev", MaxChargeW: 7000}}, Weather: &config.Weather{PVRatedW: 12345}}
	s := &Server{deps: &Deps{Cfg: cfg, CfgMu: &sync.RWMutex{}}}
	report, ok := s.CloudStatistics(context.Background(), time.Now())
	if !ok {
		t.Fatal("no report")
	}
	if report.Metrics["battery_capacity_wh"] != 35000 || report.Metrics["flexible_power_w"] != 16000 || report.Metrics["pv_rated_w"] != 12350 {
		t.Fatalf("wrong totals %+v", report.Metrics)
	}
	if _, ok := report.Metrics["pv_generation_wh"]; ok {
		t.Fatal("missing energy became zero")
	}
	cfg.Drivers[0].MaxChargeW = 0
	cfg.Drivers[0].MaxDischargeW = 0
	report, _ = s.CloudStatistics(context.Background(), time.Now())
	if _, ok := report.Metrics["flexible_power_w"]; ok {
		t.Fatal("unknown limit invented")
	}
	cfg.Nova = &config.Nova{Enabled: true}
	if _, ok := s.CloudStatistics(context.Background(), time.Now()); ok {
		t.Fatal("partner site sent statistics")
	}
}

func TestCloudStatisticsReadsClosedLedgerHourAndRoundsEnergy(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	base := now.Truncate(time.Hour).Add(-time.Hour)
	for i, value := range []float64{1000, 1123} {
		at := base.Add(time.Duration(i+1) * time.Minute).UnixMilli()
		err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: at, JSON: "{}"}, nil, []state.EnergyObservation{{AssetID: "private-hardware", DeviceID: "private-serial", AssetKind: state.AssetGridMeter, Flow: state.FlowGridImport, AtMs: at, CounterWh: &value}})
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{deps: &Deps{Cfg: &config.Config{}, CfgMu: &sync.RWMutex{}, State: st}}
	report, ok := s.CloudStatistics(context.Background(), now)
	if !ok || report.Metrics["grid_import_wh"] != 120 {
		t.Fatalf("ledger energy %+v", report)
	}
	if _, ok := report.Metrics["pv_generation_wh"]; ok {
		t.Fatal("unknown solar became zero")
	}
}
