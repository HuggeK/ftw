package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func setupTestServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	s, cfg, _ := storedConfigServer(t)
	return New(s.deps), cfg
}

func setupCall(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w, result
}

func TestSetupDraftCommitUsesTheDurableSettingsPath(t *testing.T) {
	s, cfg := setupTestServer(t)
	beforeRevision := cfg.Revision
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "New home"}}})
	if w.Code != http.StatusOK || draft["revision"] != float64(1) || draft["applied"] != false {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	if cfg.Site.Name != "Before" || cfg.Revision != beforeRevision {
		t.Fatal("editing applied settings")
	}
	if strings.Contains(w.Body.String(), "before-secret") {
		t.Fatal("draft returned a saved credential")
	}
	checked := map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"]}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", checked)
	if w.Code != http.StatusBadRequest || cfg.Site.Name != "Before" {
		t.Fatal("unconfirmed draft changed settings")
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/validate", checked)
	if w.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", w.Code, w.Body.String())
	}
	checked["confirmed"] = true
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", checked)
	if w.Code != http.StatusOK || cfg.Site.Name != "New home" || cfg.Revision != beforeRevision+1 {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	restarted, err := config.Load(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Site.Name != "New home" || restarted.EVCharger.Password != "before-secret" {
		t.Fatal("commit did not survive reload with its saved credential")
	}
	w, retried := setupCall(t, New(s.deps), http.MethodPost, "/api/setup/commit", checked)
	if w.Code != http.StatusOK || retried["saved"] != true || retried["applied"] != true || cfg.Revision != beforeRevision+1 {
		t.Fatal("retry after a lost reply did not find the saved receipt", w.Body.String())
	}
}

func TestSetupDraftResumesAcrossAPIServerRestartAndRejectsOtherSettingsEdits(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Draft"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s = New(s.deps)
	w, resumed := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || resumed["sha256"] != draft["sha256"] {
		t.Fatal("server restart lost draft")
	}
	// A concurrent Settings writer commits a different name first.
	next := *cfg
	next.Site.Name = "Another writer"
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if code := postConfig(t, s, string(raw)); code != http.StatusOK {
		t.Fatal(code)
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true})
	if w.Code != http.StatusConflict || cfg.Site.Name != "Another writer" {
		t.Fatal("stale draft overwrote another writer")
	}
}

func TestSetupDraftDoesNotClaimCommissioningFromAValidConfig(t *testing.T) {
	s, _ := setupTestServer(t)
	w, result := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || result["hardware_verified"] != false || result["applied"] != false {
		t.Fatal(w.Body.String())
	}
	w, result = setupCall(t, s, http.MethodGet, "/api/setup/schema", nil)
	if w.Code != http.StatusOK || result["schema"] == nil || result["drivers"] != "/api/drivers/catalog" {
		t.Fatal(w.Body.String())
	}
}

func TestSetupDoesNotPretendToChangeHousePasswordProtection(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, _ := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{
		"revision": 0, "patch": map[string]any{"api": map[string]any{"lan_auth": !cfg.API.LANAuth}},
	})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "house-password") {
		t.Fatal("draft accepted a password flag Settings would ignore", w.Body.String())
	}
}

func TestSetupFailedSaveLeavesLiveAndStoredConfigUntouched(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Unsaved"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s.deps.SaveConfigWithValues = func(string, *config.Config, map[string]string, []byte, func() error) error {
		return http.ErrHandlerTimeout
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true})
	if w.Code != http.StatusInternalServerError || cfg.Site.Name != "Before" {
		t.Fatal("failed commit changed live settings")
	}
	restarted, err := config.Load(s.deps.ConfigPath)
	if err != nil || restarted.Site.Name != "Before" {
		t.Fatal("failed commit changed durable settings", err)
	}
	if receipt, err := s.setup.Receipt(); err != nil || receipt != nil {
		t.Fatal("failed save left a success receipt", err)
	}
}

func TestSetupMaskedArrayRoundTripKeepsNewDraftCredentials(t *testing.T) {
	s, _ := setupTestServer(t)
	first := map[string]any{"name": "first meter", "lua": "drivers/test.lua", "is_site_meter": true,
		"capabilities": map[string]any{"standalone": true}, "config": map[string]any{"api_token": "first-draft-secret"}}
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": []any{first}}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "first-draft-secret") {
		t.Fatal(w.Body.String())
	}
	maskedDrivers := draft["config"].(map[string]any)["drivers"].([]any)
	maskedDrivers = append(maskedDrivers, map[string]any{"name": "second device", "lua": "drivers/second.lua",
		"capabilities": map[string]any{"standalone": true}, "config": map[string]any{"api_token": "second-draft-secret"}})
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": maskedDrivers}})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "first-draft-secret") || strings.Contains(w.Body.String(), "second-draft-secret") {
		t.Fatal(w.Body.String())
	}
	candidate, err := s.setup.Checked(int64(draft["base_revision"].(float64)), int64(draft["revision"].(float64)), draft["sha256"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Drivers[0].Config["api_token"] != "first-draft-secret" || candidate.Drivers[1].Config["api_token"] != "second-draft-secret" {
		t.Fatal("masked list round trip lost a new credential")
	}
}

func TestSetupReceiptAndSettingsCommitTogether(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Committed"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	check := map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", check)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s = New(s.deps)
	w, resumed := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || resumed["saved"] != true || resumed["applied"] != true || resumed["stale"] != false || resumed["hardware_verified"] != false {
		t.Fatal(w.Body.String())
	}
	if got := resumed["last_commit"].(map[string]any)["saved_revision"]; got != float64(cfg.Revision) {
		t.Fatal("receipt names a different settings revision")
	}
}

func TestSetupNewDraftDriverRenamePreservesCredential(t *testing.T) {
	s, _ := setupTestServer(t)
	first := map[string]any{"name": "first meter", "lua": "drivers/test.lua", "is_site_meter": true,
		"capabilities": map[string]any{"standalone": true}, "config": map[string]any{"api_token": "draft-only-token"}}
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": []any{first}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	drivers := draft["config"].(map[string]any)["drivers"].([]any)
	drivers[0].(map[string]any)["name"] = "Garage meter"
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	candidate, err := s.setup.Checked(int64(draft["base_revision"].(float64)), int64(draft["revision"].(float64)), draft["sha256"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Drivers[0].Config["api_token"] != "draft-only-token" {
		t.Fatal("renaming the new driver lost its draft-only credential")
	}
}

func TestSetupResumeKeepsPendingRestartVisible(t *testing.T) {
	s, _ := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"api": map[string]any{"port": 8899}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	check := map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}
	w, commit := setupCall(t, s, http.MethodPost, "/api/setup/commit", check)
	if w.Code != http.StatusOK || commit["restart_required"] != true {
		t.Fatal("initial response did not identify pending restart")
	}
	w, resumed := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	reasons, ok := resumed["restart_reasons"].([]any)
	if !ok || len(reasons) == 0 {
		t.Fatal("resume hid the pending listener restart while returning applied=true")
	}
	if resumed["restart_pending"] != true || resumed["applied"] != false {
		t.Fatal("resume claimed restart-only settings were active", w.Body.String())
	}
	w, retried := setupCall(t, New(s.deps), http.MethodPost, "/api/setup/commit", check)
	if w.Code != http.StatusOK || retried["restart_required"] != true || retried["applied"] != false {
		t.Fatal("retry hid the pending restart", w.Body.String())
	}
	if commit["saved"] != true || commit["commit"] == nil || retried["commit"] == nil {
		t.Fatal("initial reply and retry must both return a durable receipt")
	}
}

func TestSetupNewDraftKeepsTheProcessRestartPending(t *testing.T) {
	s, _ := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"api": map[string]any{"port": 8899}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/reset", map[string]any{"revision": draft["revision"]})
	if w.Code != http.StatusOK || draft["saved"] != false || draft["restart_pending"] != true || len(draft["restart_reasons"].([]any)) == 0 {
		t.Fatal("new draft hid the pending restart", w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"site": map[string]any{"name": "Next change"}}})
	if w.Code != http.StatusOK || draft["restart_pending"] != true {
		t.Fatal(w.Body.String())
	}
	w, resumed := setupCall(t, New(s.deps), http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || resumed["restart_pending"] != true || len(resumed["restart_reasons"].([]any)) == 0 {
		t.Fatal("resume hid process state", w.Body.String())
	}
}

func TestSetupSwappedDriverNamesKeepOwnPasswords(t *testing.T) {
	s, _ := setupTestServer(t)
	a := map[string]any{"name": "meter A", "lua": "drivers/test.lua", "is_site_meter": true,
		"capabilities": map[string]any{"standalone": true}, "config": map[string]any{"password": "password-A"}}
	b := map[string]any{"name": "device B", "lua": "drivers/test.lua",
		"capabilities": map[string]any{"standalone": true}, "config": map[string]any{"password": "password-B"}}
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": []any{a, b}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	drivers := draft["config"].(map[string]any)["drivers"].([]any)
	drivers[0].(map[string]any)["name"] = "device B"
	drivers[1].(map[string]any)["name"] = "meter A"
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	candidate, err := s.setup.Checked(int64(draft["base_revision"].(float64)), int64(draft["revision"].(float64)), draft["sha256"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Drivers[0].Config["password"] != "password-A" || candidate.Drivers[1].Config["password"] != "password-B" {
		t.Fatal("swapping display names crossed the drivers' credentials")
	}
}

func TestSetupLaterSetupSaveKeepsEarlierPendingRestart(t *testing.T) {
	s, _ := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"api": map[string]any{"port": 8899}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	check := map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}
	w, result := setupCall(t, s, http.MethodPost, "/api/setup/commit", check)
	if w.Code != http.StatusOK || result["restart_required"] != true {
		t.Fatal("first save had no pending restart")
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/reset", map[string]any{"revision": draft["revision"]})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"site": map[string]any{"name": "New display name"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	check = map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}
	w, result = setupCall(t, s, http.MethodPost, "/api/setup/commit", check)
	if w.Code != http.StatusOK || result["restart_required"] != true || result["applied"] != false {
		t.Fatal("a later unrelated save forgot the pending runtime change")
	}
	w, result = setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || result["restart_pending"] != true || result["applied"] != false {
		t.Fatal("resume forgot the pending restart after the second save")
	}
}
