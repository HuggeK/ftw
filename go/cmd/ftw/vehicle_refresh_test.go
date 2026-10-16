package main

import (
	"context"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVehicleRefreshUsesTheSessionCarsOwnSource(t *testing.T) {
	dir := t.TempDir()
	vehicle := func(name string, readOnly, wake bool) config.Driver {
		path := filepath.Join(dir, name+".lua")
		ro, wk := "false", "false"
		if readOnly {
			ro = "true"
		}
		if wake {
			wk = "true"
		}
		source := `DRIVER = {
 id = "test",
 capabilities = { "vehicle" },
 read_only = ` + ro + `,
 telemetry_wake = ` + wk + `,
}`
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		return config.Driver{Name: name, Lua: path}
	}
	tesla := vehicle("tesla", false, true)
	other := vehicle("other", true, false)
	// Before cars, two vehicles or two connected chargers refused the
	// refresh: nothing said which car was on which charger. Now the session's
	// car names its own sources, and only those are candidates.
	cases := []struct {
		name    string
		drivers []config.Driver
		sources []string
		want    string
		wantErr bool
	}{
		{"missing or stale soc", []config.Driver{tesla}, []string{"tesla"}, "tesla", false},
		{"another car's vehicle beside it", []config.Driver{tesla, other}, []string{"tesla"}, "tesla", false},
		{"the session's car has no source", []config.Driver{tesla}, nil, "", false},
		{"a visitor's session", []config.Driver{tesla, other}, []string{}, "", false},
		{"read only", []config.Driver{other}, []string{"other"}, "", false},
		{"no wake declaration", []config.Driver{vehicle("legacy", false, false)}, []string{"legacy"}, "", false},
		{"disabled", []config.Driver{{Name: tesla.Name, Lua: tesla.Lua, Disabled: true}}, []string{"tesla"}, "", false},
		{"observe only", []config.Driver{{Name: tesla.Name, Lua: tesla.Lua, ObserveOnly: true}}, []string{"tesla"}, "", false},
		{"unreadable metadata", []config.Driver{{Name: "gone", Lua: filepath.Join(dir, "gone.lua")}}, []string{"gone"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configuredVehicleRefreshDriver(tc.drivers, tc.sources)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got=%q err=%v", got, err)
			}
		})
	}
}

func TestVehicleRefreshPollsPromptlyAndKeepsBudgetAcrossRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vehicle.lua")
	source := `DRIVER = {
 id = "vehicle-test",
 capabilities = { "vehicle" },
 telemetry_wake = true,
}
function driver_init(config)
 host.set_make("Tesla")
 host.set_sn("VIN-A")
 host.set_poll_interval(3600000)
end
function driver_poll()
 host.emit("vehicle", {soc=100, soc_fresh=true, soc_observed_at_ms=host.unix_ms(), charging_state="Complete"})
 host.set_poll_interval(3600000)
end
function driver_command(action)
 assert(action == "wake_up")
 local allowed = host.reserve_vehicle_wake()
 host.emit_metric("reserved", allowed and 1 or 0)
 host.set_poll_interval(10)
 return true
end
function driver_default_mode() end`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tel := telemetry.NewStore()
	reg := newDriverRegistry(tel, st)
	defer reg.ShutdownAll()
	for i, name := range []string{"original", "renamed"} {
		if err := reg.Add(context.Background(), config.Driver{Name: name, Lua: path}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := reg.Send(ctx, name, []byte(`{"action":"wake_up"}`))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		expected := float64(1 - i)
		if value, _, ok := tel.LatestMetric(name, "reserved"); !ok || value != expected {
			t.Fatalf("reservation after rename: %v", value)
		}
		deadline := time.Now().Add(time.Second)
		for tel.Get(name, telemetry.DerVehicle) == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if reading := tel.Get(name, telemetry.DerVehicle); reading == nil || reading.SoC == nil || *reading.SoC != 1 {
			t.Fatal("wake interval did not reach the active poll timer")
		}
		reg.Remove(name)
	}
}
