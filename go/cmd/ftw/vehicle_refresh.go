package main

import (
	"fmt"
	"slices"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// configuredVehicleRefreshDriver is the source of the session's car that may
// be woken for a fresh reading: the first of its vehicle drivers, in config
// order, that is writable and declares telemetry_wake. The cars runtime binds
// each driver to one car (docs/cars.md), so another car's driver is never a
// candidate however many cars or chargers the site has. Freshness remains a
// requirement for planning and charge_start, not for wake.
func configuredVehicleRefreshDriver(configured []config.Driver, sources []string) (string, error) {
	for i := range configured {
		d := &configured[i]
		if d.Disabled || d.ObserveOnly || !slices.Contains(sources, d.Name) {
			continue
		}
		e, err := drivers.ParseCatalogFile(d.Lua)
		if err != nil {
			return "", fmt.Errorf("vehicle refresh: cannot read driver metadata")
		}
		if !slices.Contains(e.Capabilities, "vehicle") || e.ReadOnly || !e.TelemetryWake {
			continue
		}
		return d.Name, nil
	}
	return "", nil
}
