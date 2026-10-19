package config

import (
	"slices"
	"strings"
	"testing"
)

func TestMissingChargerConnectionWarnsWithoutRebinding(t *testing.T) {
	cfg := &Config{Loadpoints: []Loadpoint{{ID: "garage", DriverName: "old-charger", MinChargeW: 1400, MaxChargeW: 7000}}}
	applyDefaults(cfg)
	cfg.Fuse.MaxAmps, cfg.Fuse.Phases, cfg.Fuse.Voltage = 20, 3, 230
	if err := cfg.Validate(); err != nil {
		t.Fatal("valid settings should remain repairable")
	}
	if len(cfg.LoadpointWarnings) != 1 || !strings.Contains(cfg.LoadpointWarnings[0], "old-charger") {
		t.Fatal("missing charger connection was silent")
	}
	if cfg.Loadpoints[0].DriverName != "old-charger" {
		t.Fatal("validation chose a replacement")
	}
	cfg.LoadpointWarnings = append(cfg.LoadpointWarnings, "Existing charging-step correction")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LoadpointWarnings) != 2 {
		t.Fatal("validation duplicated its warning")
	}
	cfg.Loadpoints = nil
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.LoadpointWarnings, []string{"Existing charging-step correction"}) {
		t.Fatal("repair kept a stale connection warning or lost a charging-step correction")
	}
}

func TestChargerConnectionEvidenceDistinguishesConfiguredMissingAndOCPP(t *testing.T) {
	for _, tc := range []struct {
		name, bound, driver string
		ocpp                bool
		status              string
		warnings            int
	}{
		{"configured name", "charger-current", "charger-current", false, "configured", 0},
		{"old name", "charger-old", "charger-current", false, "missing", 1},
		{"no selection", "", "charger-current", false, "missing", 1},
		{"OCPP may be offline", "ocpp-garage", "", true, "unverified_ocpp", 0},
		{"OCPP does not replace an empty selection", "", "", true, "missing", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Loadpoints: []Loadpoint{{ID: "garage", DriverName: tc.bound}}, OCPP: &OCPP{Enabled: tc.ocpp}}
			if tc.driver != "" {
				cfg.Drivers = []Driver{{Name: tc.driver}}
			}
			if err := cfg.normalizeLoadpointSteps(); err != nil {
				t.Fatal(err)
			}
			if len(cfg.LoadpointWarnings) != tc.warnings {
				t.Fatal("unexpected connection warnings")
			}
			evidence := cfg.LoadpointConnections()
			if len(evidence) != 1 || evidence[0].Status != tc.status || evidence[0].DriverName != tc.bound {
				t.Fatal("connection evidence lost the exact saved name or its status")
			}
		})
	}
}
