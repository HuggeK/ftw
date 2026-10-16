package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// host.site_location gives the site's coordinates to a driver whose signed
// catalog entry lists vehicle, and nil to every other driver: a charger, a
// file that only calls itself a vehicle driver, or a site with no position.
func TestLuaSiteLocationIsForSignedVehicleDriversOnly(t *testing.T) {
	signed := func(caps ...string) *RuntimePolicy {
		return &RuntimePolicy{PackageID: "com.sourceful.driver.car", Version: "0.1.0",
			ArtifactSHA256: strings.Repeat("b", 64), RuntimeABI: "gopher-lua-source-v1",
			HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true, Capabilities: caps}
	}
	for _, tc := range []struct {
		name   string
		policy *RuntimePolicy
		set    bool
		ok     bool
		want   string
	}{
		{"signed vehicle", signed("vehicle"), true, true, "59.5,17.25"},
		{"signed vehicle on a site with no position", signed("vehicle"), true, false, "nil"},
		{"signed vehicle on a host without it", signed("vehicle"), false, false, "nil"},
		{"signed charger", signed("ev"), true, true, "nil"},
		{"a local file that calls itself a vehicle driver", nil, true, true, "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := `DRIVER = { capabilities = { "vehicle" }, read_only = true }
function driver_init(config) end
function driver_poll()
  local lat, lon = host.site_location()
  if lat == nil then SITE = "nil" else SITE = lat .. "," .. lon end
end
function driver_default_mode() end`
			path := filepath.Join(t.TempDir(), "car.lua")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			env := NewHostEnv("car", telemetry.NewStore())
			if tc.set {
				env.SiteLocation = func() (float64, float64, bool) { return 59.5, 17.25, tc.ok }
			}
			d, err := NewLuaDriverWithPolicy(path, env, tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Cleanup()
			if err := d.Init(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := d.L.GetGlobal("SITE").String(); got != tc.want {
				t.Fatalf("site_location %s, want %s", got, tc.want)
			}
		})
	}
}
