package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
)

func TestRetainedChargerBindingNeverInventsZeroEVPower(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		cfg := &config.Config{Drivers: []config.Driver{{Name: "site", Lua: "drivers/meter.lua", IsSiteMeter: true}, {Name: "charger-current", Lua: "drivers/charger.lua", Disabled: disabled}},
			Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "charger-old"}}}
		if disabled {
			cfg.Loadpoints[0].DriverName = "charger-current"
		}
		tel := telemetry.NewStore()
		tel.Update("site", telemetry.DerMeter, 4000, nil, nil)
		tel.RecordDriverSuccess("site")
		if !disabled {
			tel.Update("charger-current", telemetry.DerEV, 1000, nil, nil)
			tel.RecordDriverSuccess("charger-current")
		}
		r := tel.LiveMeasurementNow("site", historyMeasurementOptions(cfg, measurementCatalog()))
		if r.Valid || r.Known[telemetry.DerEV] || r.Reason != "missing:"+cfg.Loadpoints[0].DriverName+":ev" {
			t.Fatal("an unresolved EV flow became a valid household balance")
		}
		cfg.Loadpoints[0].DriverName = "charger-current"
		cfg.Drivers[1].Disabled = false
		tel.Update("charger-current", telemetry.DerEV, 1000, nil, nil)
		tel.RecordDriverSuccess("charger-current")
		r = tel.LiveMeasurementNow("site", historyMeasurementOptions(cfg, measurementCatalog()))
		if !r.Valid || r.HouseholdW != 3000 || r.EVW != 1000 {
			t.Fatal("an explicit repair with fresh EV evidence did not restore the balance")
		}
	}
}
