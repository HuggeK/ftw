package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A driver that declares one control, records what it was commanded and
// counts its own default-mode calls, so a test can tell "Core sent it" from
// "Core said it sent it".
const controlProbeLua = `DRIVER = {
  id      = "probe",
  name    = "Probe",
  version = "1.0.0",
  controls = {
    {
      id       = "set_offset",
      label    = "Offset",
      evidence = "readback",
      input    = { type = "number", min = -3, max = 3, step = 1, unit = "C" },
    },
  },
}

local applied   = nil
local defaulted = 0

function driver_init(config)
    host.set_make("Probe")
    -- The default first poll is 5 s away, which would make every assertion
    -- here a five-second wait for a value that was already set.
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "C") end
    host.emit_metric("defaulted", defaulted, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_offset" then
        applied = tonumber(cmd and (cmd.offset or cmd.value))
        return true
    end
    return false
end

function driver_default_mode()
    defaulted = defaulted + 1
    applied   = 0
end
`

const controlTypesProbeLua = `DRIVER = {
  id      = "probe_types",
  name    = "Probe types",
  version = "1.0.0",
  controls = {
    { id = "set_boost", input = { type = "boolean" } },
    { id = "set_mode", input = { type = "string" } },
  },
}

local applied = 0

function driver_init(config)
    host.set_make("Probe types")
    host.set_poll_interval(100)
end

function driver_poll()
    host.emit_metric("applied", applied, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_boost" then
        if cmd.value == true then applied = 1 else applied = 2 end
        return true
    end
    if action == "set_mode" then
        if cmd.value == "eco" then applied = 3 else applied = 4 end
        return true
    end
    return false
end

function driver_default_mode()
    applied = 0
end
`

const controlSafetyProbeLua = `DRIVER = {
  id      = "probe_safety",
  name    = "Probe safety",
  version = "1.0.0",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
    { id = "set_offset_fail", input = { type = "number", min = -3, max = 3 } },
  },
}

local applied   = nil
local defaulted = 0

function driver_init(config)
    host.set_make("Probe safety")
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "n") end
    host.emit_metric("defaulted", defaulted, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_offset" then
        applied = tonumber(cmd.value)
        return true
    end
    if action == "set_offset_fail" then
        applied = tonumber(cmd.value)
        host.sleep(100)
        return false
    end
    return false
end

function driver_default_mode()
    defaulted = defaulted + 1
    host.emit_metric("default_started", defaulted, "n")
    host.sleep(200)
    applied = 0
end
`

func controlServer(t *testing.T) (*Server, *telemetry.Store) {
	return controlServerWithLua(t, controlProbeLua)
}

func controlServerWithLua(t *testing.T, source string) (*Server, *telemetry.Store) {
	t.Helper()
	dir := t.TempDir()
	lua := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(lua, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	cfg := config.Driver{Name: "heat", Lua: lua}
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("add driver: %v", err)
	}
	t.Cleanup(reg.ShutdownAll)
	srv := New(&Deps{
		Tel:        tel,
		Registry:   reg,
		Cfg:        &config.Config{Drivers: []config.Driver{cfg}},
		CfgMu:      &sync.RWMutex{},
		DriverDir:  dir,
		ConfigPath: filepath.Join(dir, "config.yaml"),
	})
	return srv, tel
}

func post(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// waitMetric polls for a metric to reach want, so the test follows the
// driver's own poll loop rather than a sleep chosen by guess.
func waitMetric(t *testing.T, tel *telemetry.Store, driver, metric string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last float64
	for time.Now().Before(deadline) {
		if got, _, ok := tel.LatestMetric(driver, metric); ok {
			last = got
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s/%s = %v, want %v", driver, metric, last, want)
}

// The value reaches the driver, and Core clamps it to the declared bound
// rather than trusting the Lua to do it.
func TestDriverControlClampsAndReachesDriver(t *testing.T) {
	srv, tel := controlServer(t)

	rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":99,"duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied  *float64 `json:"applied"`
		Evidence string   `json:"evidence"`
		Expires  int64    `json:"expires_at_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Applied == nil || *resp.Applied != 3 {
		t.Errorf("applied = %v, want 3 (clamped from 99)", resp.Applied)
	}
	if resp.Evidence != "readback" {
		t.Errorf("evidence = %q", resp.Evidence)
	}
	if resp.Expires <= time.Now().UnixMilli() {
		t.Errorf("expires_at_ms = %d, want in the future", resp.Expires)
	}
	waitMetric(t, tel, "heat", "applied", 3)
}

// The declaration is the allowlist. A control the driver never declared is a
// 400, not a 200 for a command the Lua silently ignored.
func TestDriverControlRejectsUndeclared(t *testing.T) {
	srv, _ := controlServer(t)

	rec := post(t, srv, "/api/drivers/heat/control", `{"control":"set_fan","value":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown control = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	rec = post(t, srv, "/api/drivers/heat/control", `{"control":"set_offset"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing value = %d, want 400", rec.Code)
	}
	rec = post(t, srv, "/api/drivers/nosuch/control", `{"control":"set_offset","value":1}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown driver = %d, want 404", rec.Code)
	}
}

func TestDriverControlPreservesDeclaredBooleanAndStringValues(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlTypesProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_boost","value":true,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("boolean POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 1)
	if hold := srv.activeControlHold("heat"); hold == nil || hold.Value != true {
		t.Fatalf("boolean hold = %+v, want true", hold)
	}

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_mode","value":"eco","duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("string POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 3)
	if hold := srv.activeControlHold("heat"); hold == nil || hold.Value != "eco" {
		t.Fatalf("string hold = %+v, want eco", hold)
	}
}

func TestDriverControlHoldIsVisibleAndReleasable(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/heat", nil))
	var detail driverDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Hold == nil || detail.Hold.Control != "set_offset" {
		t.Fatalf("hold = %+v, want set_offset", detail.Hold)
	}
	value, ok := detail.Hold.Value.(float64)
	if !ok || value != 2 {
		t.Errorf("hold value = %v, want 2", detail.Hold.Value)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodDelete, "/api/drivers/heat/control", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, body %s", rec.Code, rec.Body.String())
	}
	// Releasing calls the driver's own default mode, not a value this
	// package invented.
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Errorf("hold survived release: %+v", hold)
	}
}

// The whole reason a hold is bounded: it has to end by itself. An offset that
// outlives the browser tab that set it heats a house wrong for weeks.
func TestDriverControlHoldExpiresIntoDefault(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":1}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "applied", 2)
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Errorf("hold survived expiry: %+v", hold)
	}
}

// Replacing a hold must not leave the old timer able to default the device
// out from under the new setting.
func TestDriverControlReplacingHoldCancelsTheOldTimer(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":1,"duration_s":1}`); rec.Code != http.StatusOK {
		t.Fatalf("first POST = %d", rec.Code)
	}
	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":-2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("second POST = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "applied", -2)

	// Past when the first hold would have fired.
	time.Sleep(1500 * time.Millisecond)
	if got, _, ok := tel.LatestMetric("heat", "defaulted"); ok && got != 0 {
		t.Errorf("defaulted = %v, want 0 — the replaced timer still fired", got)
	}
	if got, _, ok := tel.LatestMetric("heat", "applied"); !ok || got != -2 {
		t.Errorf("applied = %v, want -2 to survive", got)
	}
}

func TestDriverControlAmbiguousCommandRestoresDefault(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/heat/control", strings.NewReader(
		`{"control":"set_offset_fail","value":2,"duration_s":600}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ambiguous POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Fatalf("ambiguous command left hold active: %+v", hold)
	}
	waitMetric(t, tel, "heat", "applied", 0)
}

func TestDriverControlDefaultPathClearsHold(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	if err := srv.SendDriverDefault(context.Background(), "heat"); err != nil {
		t.Fatalf("SendDriverDefault = %v", err)
	}
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Fatalf("default path left hold active: %+v", hold)
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
}

// Expiry must hold the per-driver lock through the actual default command.
// Otherwise a replacement can be sent after the old hold is deleted but
// before its default reaches the device, and the old default wins last.
func TestDriverControlSerializesExpiryAndReplacement(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":1,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	state := srv.controlState("heat")
	state.mu.Lock()
	hold := state.hold
	state.mu.Unlock()
	if hold == nil {
		t.Fatal("missing hold")
	}

	done := make(chan struct{})
	go func() {
		srv.expireControlHold("heat", state, hold)
		close(done)
	}()
	waitMetric(t, tel, "heat", "default_started", 1)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":-2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("replacement POST = %d, body %s", rec.Code, rec.Body.String())
	}
	<-done
	waitMetric(t, tel, "heat", "applied", -2)
}
