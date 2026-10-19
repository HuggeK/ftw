// measurement-api serves the real Core read handlers with synthetic telemetry
// and temporary SQLite data for the paired Cloud MCP integration tests. It
// opens no driver, controls no device and reads no installed configuration.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

func main() {
	if len(os.Args) != 2 {
		panic("choose missing, disabled or repaired fixture")
	}
	mode := os.Args[1]
	if mode != "missing" && mode != "disabled" && mode != "repaired" {
		panic("unknown measurement fixture")
	}
	dir, err := os.MkdirTemp("", "ftw-measurement-api-")
	if err != nil {
		panic("temporary state unavailable")
	}
	defer os.RemoveAll(dir)
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		panic("temporary state unavailable")
	}
	defer st.Close()
	if st.EnableHistoryAggregation() != nil {
		panic("history aggregation unavailable")
	}
	base := time.Now().UTC().Truncate(5 * time.Minute).Add(-20 * time.Minute).UnixMilli()
	for i := range 2 {
		counter := 100 + float64(i)*12
		if err := st.EnqueueTelemetryTick(nil, []state.Sample{{Driver: "charger-current", Metric: "ev_w", TsMs: base + int64(i)*60000, Value: 1000 + float64(i)*1000, Unit: "W"}}, []state.EnergyObservation{{AssetID: "fixture-asset", DeviceID: "fixture-device", AssetKind: state.AssetGridMeter, Label: "fixture-site", Flow: state.FlowGridImport, AtMs: base + int64(i)*60000, CounterWh: &counter}}); err != nil {
			panic("could not seed measurement history")
		}
	}
	if st.FlushHistory(context.Background()) != nil {
		panic("could not flush measurement history")
	}
	cfg := &config.Config{Drivers: []config.Driver{{Name: "site", IsSiteMeter: true}, {Name: "charger-current", Disabled: mode == "disabled"}}, Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "charger-current"}}}
	if mode == "missing" {
		cfg.Loadpoints[0].DriverName = "charger-old"
	}
	tel := telemetry.NewStore()
	tel.Update("site", telemetry.DerMeter, 4000, nil, nil)
	tel.RecordDriverSuccess("site")
	if mode != "disabled" {
		data, _ := json.Marshal(map[string]any{"power_observed_at": time.Now().UTC().Format(time.RFC3339Nano)})
		tel.Update("charger-current", telemetry.DerEV, 1000, nil, data)
		tel.RecordDriverSuccess("charger-current")
	}
	srv := api.New(&api.Deps{State: st, Tel: tel, Cfg: cfg, CfgMu: &sync.RWMutex{}, Ctrl: &control.State{SiteMeterDriver: "site"}, CtrlMu: &sync.Mutex{}, CapMu: &sync.RWMutex{}, Version: "0.153.0-test",
		HistoryMeasurementSources: func() telemetry.ForecastOptions {
			return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: cfg.Loadpoints[0].DriverName, DerType: telemetry.DerEV}}}
		}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := apiauth.Caller{Kind: apiauth.KindAgent, Role: apiauth.RoleAgent, Subject: "agent:fixture", Scopes: apiauth.NewScopeSet(protocol.ScopeDiagRun)}
		r = r.WithContext(apiauth.WithCaller(r.Context(), caller))
		srv.ServeAgentRead(w, r, protocol.APIRead{MaxBytes: protocol.AgentAPIPageBytes})
	}))
	defer server.Close()
	// The parent test consumes this on a private pipe; it never logs the URL.
	fmt.Println(server.URL)
	// Closing stdin ends the fixture and removes all temporary state.
	_, _ = os.Stdin.Read(make([]byte, 1))
}
