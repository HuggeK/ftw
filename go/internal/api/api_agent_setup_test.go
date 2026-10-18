package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

func agentSetupCall(t *testing.T, s *Server, message string, body any, scopes []string, guard func() error) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := agentReadRequest("/api/setup/draft", scopes...)
	s.ServeAgentSetup(w, r, message, protocol.SetupCall{JSON: raw}, guard)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return w, result
}

func TestAgentSetupUsesItsOwnGrantAndDurableSettings(t *testing.T) {
	s, cfg := setupTestServer(t)
	ok := func() error { return nil }
	w, _ := agentSetupCall(t, s, protocol.MsgSetupEdit, map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Denied"}}}, []string{protocol.ScopeDiagRun}, ok)
	if w.Code != http.StatusForbidden || cfg.Site.Name != "Before" {
		t.Fatal(w.Body.String())
	}
	w, draft := agentSetupCall(t, s, protocol.MsgSetupEdit, map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Assistant setup"}}}, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusOK || cfg.Site.Name != "Before" {
		t.Fatal(w.Body.String())
	}
	check := map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}
	w, result := agentSetupCall(t, s, protocol.MsgSetupCommit, check, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusOK || cfg.Site.Name != "Assistant setup" || result["saved"] != true || result["hardware_verified"] != false {
		t.Fatal(w.Body.String())
	}
	w, retry := agentSetupCall(t, s, protocol.MsgSetupCommit, check, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusOK || retry["commit"] == nil || retry["live_revision"] != result["live_revision"] {
		t.Fatal(w.Body.String())
	}
}

func TestAgentSetupRechecksAuthorityAtTheFinalSave(t *testing.T) {
	s, cfg := setupTestServer(t)
	ok := func() error { return nil }
	w, draft := agentSetupCall(t, s, protocol.MsgSetupEdit, map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Revoked"}}}, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	checks := 0
	w, _ = agentSetupCall(t, s, protocol.MsgSetupCommit, map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true}, []string{protocol.ScopeSetupWrite}, func() error {
		checks++
		if checks == 2 {
			return errors.New("grant revoked")
		}
		return nil
	})
	if w.Code < 400 || cfg.Site.Name != "Before" || checks != 2 {
		t.Fatal("revoked setup reached Settings", w.Body.String())
	}
	if receipt, err := s.setup.Receipt(); err != nil || receipt != nil {
		t.Fatal("revoked save left a receipt", err)
	}
}

func TestSetupGatewayKeepsTheAgentIdentityAndRejectsGeneralAPIWrites(t *testing.T) {
	s, _ := setupTestServer(t)
	r := agentReadRequest("/api/setup/draft", protocol.ScopeSetupWrite)
	w := httptest.NewRecorder()
	s.ServeAgentSetup(w, r, "api.req", protocol.SetupCall{}, func() error { return nil })
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Body.String())
	}
	caller, _ := apiauth.FromRequest(r)
	if caller.Kind != apiauth.KindAgent || caller.Role != apiauth.RoleAgent {
		t.Fatal("setup promoted the caller")
	}
}

func TestNarrowSetupReadIncludesTheMetadataItNeeds(t *testing.T) {
	s, _ := setupTestServer(t)
	w, result := agentSetupCall(t, s, protocol.MsgSetupSchema, map[string]any{}, []string{protocol.ScopeConfigRead}, func() error { return nil })
	if w.Code != http.StatusOK || result["driver_catalog"] == nil || result["runtime_timezone"] == "" {
		t.Fatal(w.Body.String())
	}
	zones, ok := result["price_zones"].([]any)
	if !ok || len(zones) == 0 {
		t.Fatal("setup did not include its price choices")
	}
	found := false
	for _, item := range zones {
		if item.(map[string]any)["code"] == "SE3" {
			found = true
		}
	}
	if !found {
		t.Fatal("setup's price choices differ from the normal API")
	}
}
