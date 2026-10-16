package drivers

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The vehicle drivers that say where a car's cable and the car are, run in
// this host through to the observations the cars runtime reads. testdata
// holds copies of srcfl/device-drivers#182 (home_assistant_vehicle 0.1.0,
// tesla_cloud 0.2.0, teslamate_vehicle 0.2.0) until the driver pin moves.

func vehicleTestdata(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", name+".lua")
}

// A signed vehicle driver's policy: host.site_location answers it.
func signedVehicle(id string) *RuntimePolicy {
	return &RuntimePolicy{PackageID: "com.sourceful.driver." + id, Version: "0.1.0",
		ArtifactSHA256: strings.Repeat("c", 64), RuntimeABI: "gopher-lua-source-v1",
		HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
		Permissions:  map[string]bool{"mqtt.subscribe": true, "http.get": true, "http.post": true},
		AuthPostPath: "/oauth2/v3/token", Capabilities: []string{"vehicle"}}
}

// vehicleEnv is a host that has run for a minute. The drivers read a
// host.millis() of 0 as "never read", which a real box is long past by its
// first reading.
func vehicleEnv(name string) *HostEnv {
	env := NewHostEnv(name, telemetry.NewStore())
	env.Start = time.Now().Add(-time.Minute)
	return env
}

// Uppsala Cathedral, a public landmark, stands in for the site.
func uppsala() (float64, float64, bool) { return 59.858, 17.6336, true }

// observe waits for the driver's observation: the store takes an emit as it
// lands, and the health that lists a driver comes with it.
func observe(t *testing.T, env *HostEnv) (telemetry.VehicleObservation, *telemetry.DerReading) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		env.Telemetry.DriverHealthMut(env.DriverName).RecordSuccess()
		for _, o := range telemetry.VehicleObservations(env.Telemetry) {
			if o.Driver == env.DriverName {
				return o, env.Telemetry.Get(env.DriverName, telemetry.DerVehicle)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no observation", env.DriverName)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func noCoordinates(t *testing.T, r *telemetry.DerReading, coords ...string) {
	t.Helper()
	for _, c := range coords {
		if strings.Contains(string(r.Data), c) {
			t.Fatalf("the car's coordinates left the driver: %s", r.Data)
		}
	}
}

func TestHomeAssistantVehicleSaysWhereTheCarIs(t *testing.T) {
	env := vehicleEnv("tesla-ha")
	mq := &fakeMQTT{}
	env.WithMQTT(mq)
	d, err := NewLuaDriver(vehicleTestdata("home_assistant_vehicle"), env)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"topic": "ftw/vehicle/tesla-ha/state",
		"vin": "5YJ3E1EA1KF000000", "make": "Tesla"}); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixMilli()) / 1000
	since := now - 20
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	mq.Push("ftw/vehicle/tesla-ha/state", `{"available":true,"soc_pct":14.0,"charge_limit_pct":80.0,`+
		`"charging_state":"Stopped","observed_at":`+f(now-5)+`,"source_age_s":5.0,"plugged":true,`+
		`"plugged_since":`+f(since)+`,"at_site":true}`)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, _ := observe(t, env)
	if o.SoC != 0.14 || o.ChargeLimit != 0.8 || o.Plugged == nil || !*o.Plugged || o.AtSite == nil || !*o.AtSite {
		t.Fatalf("observation %+v", o)
	}
	if d := o.PluggedSince.Sub(time.UnixMilli(int64(since * 1000))); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("plugged since %v, want last_changed", o.PluggedSince)
	}
	if age := time.Since(o.At); age < 4*time.Second || age > 7*time.Second {
		t.Fatalf("the reading's own time: %v old", age)
	}
}

func TestTeslaMateVehicleSaysWhereTheCarIs(t *testing.T) {
	env := vehicleEnv("teslamate")
	mq := &fakeMQTT{}
	env.WithMQTT(mq)
	env.SiteLocation = uppsala
	d, err := NewLuaDriverWithPolicy(vehicleTestdata("teslamate_vehicle"), env, signedVehicle("teslamate_vehicle"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"vin": "5YJ3E1EA1KF000000", "car_id": 1}); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"state", "online"}, {"healthy", "true"}, {"battery_level", "14"},
		{"plugged_in", "false"}, {"latitude", "59.8583"}, {"longitude", "17.6340"}} {
		mq.Push("teslamate/cars/1/"+kv[0], kv[1])
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A live update: the cable goes in, with its healthy message.
	mq.Push("teslamate/cars/1/plugged_in", "true")
	mq.Push("teslamate/cars/1/healthy", "true")
	in := time.Now()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, r := observe(t, env)
	if o.Plugged == nil || !*o.Plugged || o.AtSite == nil || !*o.AtSite || o.SoC != 0.14 {
		t.Fatalf("observation %+v", o)
	}
	if d := o.PluggedSince.Sub(in); d < -time.Second || d > time.Second {
		t.Fatalf("plugged since %v, want about %v", o.PluggedSince, in)
	}
	noCoordinates(t, r, "59.858", "17.634")
}

func TestTeslaCloudSaysWhereTheCarIs(t *testing.T) {
	src, err := os.ReadFile(vehicleTestdata("tesla_cloud"))
	if err != nil {
		t.Fatal(err)
	}
	// Tesla's API, stubbed in Lua: the car is online, on an IEC cable, at
	// Stockholm Central.
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	stub := `
host.http_post = function(url) return '{"access_token":"a","refresh_token":"r","expires_in":28800}' end
host.http_get = function(url)
  if url:find("vehicle_data", 1, true) then
    assert(url:find("location_data", 1, true), "no location_data asked: " .. url)
    return '{"response":{"vin":"5YJ3E1EA1KF000000","state":"online","charge_state":{"battery_level":14,` +
		`"charge_limit_soc":80,"charging_state":"Stopped","conn_charge_cable":"IEC","timestamp":` + ts + `},` +
		`"drive_state":{"latitude":59.3301,"longitude":18.0586,"timestamp":` + ts + `}}}'
  end
  return '{"response":{"vin":"5YJ3E1EA1KF000000","state":"online"}}'
end
`
	path := filepath.Join(t.TempDir(), "tesla_cloud.lua")
	if err := os.WriteFile(path, append([]byte(stub), src...), 0600); err != nil {
		t.Fatal(err)
	}
	env := vehicleEnv("tesla-cloud")
	env.SiteLocation = uppsala
	d, err := NewLuaDriverWithPolicy(path, env, signedVehicle("tesla_cloud"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"client_id": "c", "refresh_token": "r",
		"vin": "5YJ3E1EA1KF000000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, r := observe(t, env)
	if o.Plugged == nil || !*o.Plugged || o.AtSite == nil || *o.AtSite || !o.PluggedSince.IsZero() || o.SoC != 0.14 {
		t.Fatalf("observation %+v (data %s)", o, r.Data)
	}
	noCoordinates(t, r, "59.33", "18.05")
}
