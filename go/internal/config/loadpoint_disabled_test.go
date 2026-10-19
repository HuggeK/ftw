package config

import (
	"strings"
	"testing"
)

func TestDisabledChargerBindingWarnsAndRecovers(t *testing.T) {
	cfg := &Config{Drivers: []Driver{{Name: "charger", Disabled: true}},
		Loadpoints: []Loadpoint{{ID: "garage", DriverName: "charger"}}, OCPP: &OCPP{Enabled: true}}
	for range 2 {
		if err := cfg.normalizeLoadpointSteps(); err != nil {
			t.Fatal(err)
		}
		if cfg.LoadpointConnections()[0].Status != "disabled" || len(cfg.LoadpointWarnings) != 1 || !strings.Contains(cfg.LoadpointWarnings[0], "disabled") {
			t.Fatal("a disabled saved charger binding did not keep one actionable warning")
		}
		if cfg.Loadpoints[0].DriverName != "charger" || !cfg.Drivers[0].Disabled {
			t.Fatal("validation changed equipment selection or enabled a driver")
		}
	}
	cfg.Drivers[0].Disabled = false
	if err := cfg.normalizeLoadpointSteps(); err != nil {
		t.Fatal(err)
	}
	if cfg.LoadpointConnections()[0].Status != "configured" || len(cfg.LoadpointWarnings) != 0 {
		t.Fatal("re-enabling left an obsolete warning")
	}
	cfg.Drivers = nil
	cfg.OCPP.Enabled = false
	if err := cfg.normalizeLoadpointSteps(); err != nil {
		t.Fatal(err)
	}
	if cfg.LoadpointConnections()[0].Status != "missing" || len(cfg.LoadpointWarnings) != 1 {
		t.Fatal("removal did not replace the warning")
	}
	cfg.Loadpoints = nil
	if err := cfg.normalizeLoadpointSteps(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LoadpointWarnings) != 0 {
		t.Fatal("removing the charger kept an obsolete warning")
	}
}
