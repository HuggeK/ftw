package api

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/protocol"
)

func TestDiagnosticCatalogueIncludesExactSavedChargerConnections(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{Name: "charger-current"}},
		Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "charger-old"}, {ID: "driveway", DriverName: "charger-current"}}}
	srv := New(&Deps{Cfg: cfg, CfgMu: &sync.RWMutex{}, Version: "0.153.0-beta.1"})
	answer, _ := diagnosticRead(t, srv, protocol.AgentAPICatalogPath, protocol.APIRead{})
	var page struct {
		Troubleshooting struct {
			Connections []config.LoadpointConnection `json:"charger_connections"`
		} `json:"troubleshooting"`
	}
	if err := json.Unmarshal(answer.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	connections := page.Troubleshooting.Connections
	if len(connections) != 2 || connections[0].DriverName != "charger-old" || connections[0].Status != "missing" ||
		connections[1].DriverName != "charger-current" || connections[1].Status != "configured" {
		t.Fatal("diagnostic evidence lost the saved name or confused a name binding with device health")
	}
}
