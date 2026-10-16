package main

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func newDriverRegistry(tel *telemetry.Store, st *state.Store) *drivers.Registry {
	reg := drivers.NewRegistry(tel)
	// Install both callbacks before Add can initialize or poll any driver.
	// Rotations keep their own KV rows so they do not apply the whole config
	// or restart the driver that just refreshed its credential.
	driverSecretKey := func(owner, key string) string {
		return config.DriverSecretStateKey(owner, key)
	}
	reg.SecretPersister = func(owner, key, value string) error {
		return st.SaveDriverSecret(owner, key, value)
	}
	reg.SecretOverride = func(owner, key string) (string, bool) {
		return st.LoadConfig(driverSecretKey(owner, key))
	}
	reg.VehicleWakeReservation = func(makeName, serial string) (bool, time.Duration, error) {
		return st.ReserveVehicleWake(state.ResolveDeviceID(makeName, serial, "", ""), time.Now())
	}
	return reg
}

// Where the settings page puts the weather pin when nobody has moved it.
const unsetPinLat, unsetPinLon = 59.3293, 18.0686

// siteLocation is the site's position: the weather pin on the box's map.
// The untouched pin and 0,0 are no position. A car measured against a pin in
// central Stockholm would read as elsewhere and turn the only car into a
// visitor.
func siteLocation(cfg *config.Config) (lat, lon float64, ok bool) {
	if cfg == nil || cfg.Weather == nil {
		return 0, 0, false
	}
	lat, lon = cfg.Weather.Latitude, cfg.Weather.Longitude
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 ||
		(lat == 0 && lon == 0) || (lat == unsetPinLat && lon == unsetPinLon) {
		return 0, 0, false
	}
	return lat, lon, true
}
