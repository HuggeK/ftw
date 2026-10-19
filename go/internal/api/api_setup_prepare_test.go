package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/protocol"
)

func TestSetupConnectionTestUsesOnlyItsDraftAndKeepsRotatedCredentials(t *testing.T) {
	s, live := setupTestServer(t)
	dir := t.TempDir()
	luaPath := filepath.Join(dir, "draft_probe.lua")
	if err := os.WriteFile(luaPath, []byte(`
DRIVER = {
 id="draft_probe",
 config_secrets={"refresh_token"}
}
function driver_init(config)
  if config.refresh_token ~= "draft-secret" then error("WRONG CREDENTIAL") end
  host.persist_secret("refresh_token", "rotated-draft-secret")
  host.emit_metric("draft_connected", 1)
end
function driver_poll() return 100 end
function driver_default_mode() error("MUST NOT CONTROL") end
function driver_command() error("MUST NOT CONTROL") end
function driver_cleanup() error("MUST NOT CONTROL") end
`), 0600); err != nil {
		t.Fatal(err)
	}
	s.deps.UserDriverDir = dir
	w, draft := setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{
		"drivers": []any{map[string]any{"name": "new-meter", "lua": luaPath, "is_site_meter": true, "capabilities": map[string]any{"standalone": true}, "config": map[string]any{"refresh_token": "draft-secret"}}},
	}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, action := range []map[string]any{
		{"revision": draft["revision"], "action": "test", "driver": "new-meter", "confirmed": false},
		{"revision": 0, "action": "test", "driver": "new-meter", "confirmed": true},
		{"revision": draft["revision"], "action": "connect", "driver": "missing", "confirmed": true},
	} {
		w, _ = setupCall(t, s, "POST", "/api/setup/draft", action)
		if w.Code < 400 {
			t.Fatal("invalid action ran", w.Body.String())
		}
	}
	w, tested := setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": draft["revision"], "action": "test", "driver": "new-meter", "confirmed": true})
	if w.Code != 200 || tested["revision"] != float64(2) || !strings.Contains(w.Body.String(), "draft_connected") || strings.Contains(w.Body.String(), "draft-secret") {
		t.Fatal("draft connection did not retain its own token rotation", w.Body.String())
	}
	if live.EVCharger.Password != "before-secret" {
		t.Fatal("probe changed live credentials")
	}
	w, resumed := setupCall(t, New(s.deps), "GET", "/api/setup/draft", nil)
	if w.Code != 200 || resumed["revision"] != float64(2) || strings.Contains(w.Body.String(), "draft-secret") {
		t.Fatal(w.Body.String())
	}
	stored, err := s.setup.Read(live)
	if err != nil || !strings.Contains(string(stored.Config), "rotated-draft-secret") {
		t.Fatal("rotated token did not survive resume", err)
	}
}

func TestSetupPreparationHonoursItsGrantBeforeAnyAction(t *testing.T) {
	s, _ := setupTestServer(t)
	for _, scopes := range [][]string{{protocol.ScopeDiagRun}, {protocol.ScopeSetupWrite}} {
		guard := func() error { return errors.New("revoked") }
		w, _ := agentSetupCall(t, s, protocol.MsgSetupEdit, map[string]any{"revision": 0, "action": "discover", "confirmed": true}, scopes, guard)
		if w.Code != http.StatusForbidden {
			t.Fatal("revoked discovery proceeded", w.Body.String())
		}
	}
}

func TestSetupGuideResumesAnswersWithoutInventingVerification(t *testing.T) {
	s, _ := setupTestServer(t)
	patch := map[string]any{"forecast_priors": map[string]any{"annual_kwh": 16000, "annual_basis": "grid_import", "source": "owner electricity bill"}}
	w, draft := setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": 0, "patch": patch})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, resumed := setupCall(t, New(s.deps), "GET", "/api/setup/draft", nil)
	guide := resumed["guide"].(map[string]any)
	prior := guide["forecast_priors"].(map[string]any)
	if resumed["revision"] != draft["revision"] || prior["load_prior_eligible"] != false || prior["reason"] != "total_consumption_needed" || resumed["hardware_verified"] != false {
		t.Fatal("resume confused purchases with consumption or configuration with verification", w.Body.String())
	}
	encoded, _ := json.Marshal(guide)
	if !strings.Contains(string(encoded), "annual_consumption") {
		t.Fatal("missing actionable next question")
	}
}

func TestSetupDoesNotForkARunningOAuthTokenFamily(t *testing.T) {
	s, live := setupTestServer(t)
	file := filepath.Join(t.TempDir(), "oauth.lua")
	if err := os.WriteFile(file, []byte("DRIVER = {\n id=\"oauth\"\n}\nfunction driver_init() error(\"PROBE MUST NOT START\") end"), 0600); err != nil {
		t.Fatal(err)
	}
	s.deps.UserDriverDir = filepath.Dir(file)
	live.Drivers = []config.Driver{{Name: "meter", Lua: file, IsSiteMeter: true, Config: map[string]any{"refresh_token": "live-token-secret"}, Capabilities: config.Capabilities{Standalone: true}}}
	if err := config.SaveStored(s.deps.State, s.deps.ConfigPath, live); err != nil {
		t.Fatal(err)
	}
	w, draft := setupCall(t, s, "GET", "/api/setup/draft", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w, result := setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": draft["revision"], "action": "test", "driver": "meter", "confirmed": true})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	answer := result["result"].(map[string]any)
	if answer["state"] != "shared_live_credentials" || answer["probe_was_run"] != false || answer["draft_connection_verified"] != false || strings.Contains(w.Body.String(), "live-token-secret") {
		t.Fatal(w.Body.String())
	}
}

func TestSetupScrubsBlankedSecretsFromVendorErrors(t *testing.T) {
	s, _ := setupTestServer(t)
	cfg := config.NewSetup()
	cfg.Weather = &config.Weather{APIKey: "blanked-api-secret"}
	values := s.setupSecretValues(cfg)
	if got := scrubSetupResult("vendor echoed blanked-api-secret", values); strings.Contains(got.(string), "blanked-api-secret") {
		t.Fatal(got)
	}
}
