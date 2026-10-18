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

func TestSetupFailedSaveLeavesLiveAndStoredConfigUntouched(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Unsaved"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s.deps.SaveConfigWithValues = func(string, *config.Config, map[string]string) error { return http.ErrHandlerTimeout }
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
