package drivers

import (
	"context"
	"fmt"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestControlAuthorityFollowsLoadedDriver(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	t.Cleanup(r.ShutdownAll)
	if r.CanControl("car") {
		t.Fatal("missing driver can control")
	}
	for _, tc := range []struct{ readOnly, observeOnly, want bool }{
		{true, false, false}, {false, true, false}, {false, false, true},
	} {
		path := writeTestDriver(t, fmt.Sprintf(`
DRIVER = { id = "car", capabilities = { "vehicle" }, read_only = %t }
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() end
`, tc.readOnly))
		if err := r.Add(context.Background(), config.Driver{Name: "car", Lua: path, ObserveOnly: tc.observeOnly}); err != nil {
			t.Fatal(err)
		}
		if got := r.CanControl("car"); got != tc.want {
			t.Fatalf("read_only=%v observe_only=%v: got %v", tc.readOnly, tc.observeOnly, got)
		}
		r.Remove("car")
		if r.CanControl("car") {
			t.Fatal("removed driver can control")
		}
	}
}
