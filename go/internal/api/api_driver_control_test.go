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

func controlServer(t *testing.T) (*Server, *telemetry.Store) {
	t.Helper()
	dir := t.TempDir()
	lua := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(lua, []byte(controlProbeLua), 0o600); err != nil {
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
	if detail.Hold.Value == nil || *detail.Hold.Value != 2 {
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
