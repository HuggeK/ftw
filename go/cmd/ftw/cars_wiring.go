package main

import (
	"slices"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// carProfiles turns config.yaml's vehicles[] into the cars runtime's
// profiles. Each profile the box has not seen becomes a car (docs/cars.md).
func carProfiles(vehicles []config.Vehicle) []cars.Profile {
	out := make([]cars.Profile, 0, len(vehicles))
	for _, v := range vehicles {
		target := v.TargetSoC
		if target <= 0 && v.TargetSoCPct > 0 {
			target = v.TargetSoCPct / 100
		}
		out = append(out, cars.Profile{
			ID: v.ID, Name: v.Name, CapacityWh: v.CapacityWh,
			Identifiers: v.Identifiers, SurplusOnly: v.SurplusOnly, TargetSoC: target,
		})
	}
	return out
}

// evCarInputs is what a plan takes from the session's car.
type evCarInputs struct {
	// CapacityWh is the car's, 60 kWh when nobody knows it.
	CapacityWh float64
	// InitialSoC is the level the app shows: the car's own number while it
	// is exact, its last reading plus the energy since after.
	InitialSoC float64
	// ChargeLimit caps the plan only while the level is the car's own and
	// still exact. 0 is unknown.
	ChargeLimit float64
	// Source names where the level comes from, for the planner's log.
	Source string
}

// evPlanCar reads the session's car off the loadpoint state the cars runtime
// keeps (docs/cars.md). Before cars the planner read the charger's configured
// capacity here, whatever car was plugged in.
func evPlanCar(st loadpoint.State, rt *cars.Runtime) evCarInputs {
	// The charge limit is the one the reserve reads too (loadpoint.DeadlineNeed).
	in := evCarInputs{CapacityWh: st.VehicleCapacityWh, InitialSoC: st.CurrentSoC, Source: "session", ChargeLimit: st.CarChargeLimit}
	if rt == nil {
		return in
	}
	if view, ok := rt.Session(st.ID); ok && view.Source != "" && !st.SoCUnknown {
		in.Source = "car:" + view.Source
		in.InitialSoC = view.Shown
	}
	return in
}

// configuredVehicleDrivers names the enabled drivers whose catalog entry
// declares the vehicle capability: the sources a car can be bound to before
// they report.
func configuredVehicleDrivers(configured []config.Driver) []string {
	var out []string
	for _, d := range configured {
		if d.Disabled {
			continue
		}
		e, err := drivers.ParseCatalogFile(d.Lua)
		if err != nil || !slices.Contains(e.Capabilities, "vehicle") {
			continue
		}
		out = append(out, d.Name)
	}
	return out
}

// loadpointSettingsChanges lists the loadpoint settings a config.yaml change
// moved, for the car each charger shows (docs/cars.md).
func loadpointSettingsChanges(oldCfg, newCfg *config.Config) []cars.SettingsChange {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	before := map[string]config.Loadpoint{}
	for _, lp := range oldCfg.Loadpoints {
		before[lp.ID] = lp
	}
	var out []cars.SettingsChange
	for _, lp := range newCfg.Loadpoints {
		prev, ok := before[lp.ID]
		if !ok {
			continue
		}
		c := cars.SettingsChange{LP: lp.ID}
		if lp.VehicleCapacityWh != prev.VehicleCapacityWh && lp.VehicleCapacityWh > 0 {
			c.CapacityWh = &lp.VehicleCapacityWh
		}
		if plugin, prevPlugin := pluginFraction(lp), pluginFraction(prev); plugin != prevPlugin && plugin > 0 {
			c.PluginSoC = &plugin
		}
		if lp.SurplusOnly != prev.SurplusOnly {
			v := lp.SurplusOnly
			c.SurplusOnly = &v
		}
		if c.CapacityWh != nil || c.PluginSoC != nil || c.SurplusOnly != nil {
			out = append(out, c)
		}
	}
	return out
}

func pluginFraction(lp config.Loadpoint) float64 {
	if lp.PluginSoC > 0 {
		return lp.PluginSoC
	}
	return lp.PluginSoCPct / 100
}
