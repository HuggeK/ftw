package config

import (
	"fmt"
	"strings"
)

// LoadpointConnection describes a saved name binding, not device health.
type LoadpointConnection struct {
	ID         string `json:"id"`
	DriverName string `json:"driver_name"`
	Status     string `json:"status"`
}

func (c *Config) LoadpointConnections() []LoadpointConnection {
	drivers := make(map[string]Driver, len(c.Drivers))
	for _, driver := range c.Drivers {
		drivers[driver.Name] = driver
	}
	connections := make([]LoadpointConnection, 0, len(c.Loadpoints))
	for _, lp := range c.Loadpoints {
		status := "missing"
		if driver, found := drivers[lp.DriverName]; lp.DriverName != "" && found {
			status = "configured"
			if driver.Disabled {
				status = "disabled"
			}
		} else if lp.DriverName != "" && c.OCPP != nil && c.OCPP.Enabled {
			// OCPP identities have no driver entry and may not have connected yet.
			status = "unverified_ocpp"
		}
		connections = append(connections, LoadpointConnection{ID: lp.ID, DriverName: lp.DriverName, Status: status})
	}
	return connections
}

const missingChargerConnection = "Missing charger connection: "
const disabledChargerConnection = "Disabled charger connection: "

func (c *Config) warnMissingLoadpointConnections() {
	// Recompute these warnings after a repair while retaining step corrections.
	warnings := make([]string, 0, len(c.LoadpointWarnings))
	for _, warning := range c.LoadpointWarnings {
		if !strings.HasPrefix(warning, missingChargerConnection) && !strings.HasPrefix(warning, disabledChargerConnection) {
			warnings = append(warnings, warning)
		}
	}
	for _, connection := range c.LoadpointConnections() {
		if connection.Status == "disabled" {
			warnings = append(warnings, disabledChargerConnection+fmt.Sprintf("charger %q refers to disabled driver %q. Enable that driver in Settings > Devices, select an enabled charger, or remove this charger in Settings > Chargers.", connection.ID, connection.DriverName))
			continue
		}
		if connection.Status != "missing" {
			continue
		}
		detail := fmt.Sprintf("charger %q has no charger selected.", connection.ID)
		if connection.DriverName != "" {
			detail = fmt.Sprintf("charger %q refers to %q, which is not a configured driver.", connection.ID, connection.DriverName)
		}
		warnings = append(warnings, missingChargerConnection+detail+" Select a configured charger or remove this charger in Settings > Chargers.")
	}
	c.LoadpointWarnings = warnings
}
