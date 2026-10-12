// Package localzone names the zone the box keeps its clock in.
package localzone

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Name returns the IANA name of the box's zone, or "UTC".
//
// Go names the host zone "Local" when TZ is unset, and no client can load a
// zone by that name. Clients render plan times and days in this zone, and
// persisted clock buckets keep its DST rules, so it must be a name that
// time.LoadLocation and Intl accept. UTC is valid when that is the box's
// configured zone; never guess a country.
func Name() string { return name() }

// Read once: time.Local is fixed when the process starts, so a zone changed
// on the host afterwards must not rename the one the box still runs on.
var name = sync.OnceValue(func() string {
	return resolve(os.Getenv("TZ"), time.Local.String(), "/etc/localtime", "/etc/timezone")
})

func resolve(tz, local, localtime, timezone string) string {
	candidates := []string{strings.TrimPrefix(tz, ":"), local}
	if target, err := filepath.EvalSymlinks(localtime); err == nil {
		if _, zone, ok := strings.Cut(target, "/zoneinfo/"); ok {
			candidates = append(candidates, zone)
		}
	}
	if data, err := os.ReadFile(timezone); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(data)))
	}
	for _, zone := range candidates {
		if zone == "" || zone == "Local" {
			continue
		}
		if _, err := time.LoadLocation(zone); err == nil {
			return zone
		}
	}
	return "UTC"
}
