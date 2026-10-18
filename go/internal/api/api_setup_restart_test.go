package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
)

func savedSetupForRestart(t *testing.T, s *Server) map[string]any {
	t.Helper()
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"api": map[string]any{"port": 8899}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, result := setupCall(t, s, http.MethodPost, "/api/setup/commit", map[string]any{"revision": draft["revision"], "base_revision": draft["base_revision"], "sha256": draft["sha256"], "confirmed": true})
	if w.Code != http.StatusOK || result["restart_required"] != true {
		t.Fatal(w.Body.String())
	}
	receipt := result["commit"].(map[string]any)
	return map[string]any{"saved_revision": receipt["saved_revision"], "sha256": receipt["sha256"], "confirmed": true}
}

func TestAgentSetupRestartRequiresTheConfirmedSavedReceipt(t *testing.T) {
	s, _ := setupTestServer(t)
	q := savedSetupForRestart(t, s)
	var count atomic.Int32
	done := make(chan struct{}, 2)
	s.deps.Restart = func(context.Context) error { count.Add(1); done <- struct{}{}; return nil }
	ok := func() error { return nil }
	q["confirmed"] = false
	w, _ := agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusBadRequest {
		t.Fatal(w.Body.String())
	}
	q["confirmed"] = true
	w, _ = agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeDiagRun}, ok)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Body.String())
	}
	sha := q["sha256"]
	q["sha256"] = "different"
	w, _ = agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusConflict {
		t.Fatal(w.Body.String())
	}
	q["sha256"] = sha
	w, _ = agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	w, _ = agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, ok)
	if w.Code != http.StatusAccepted {
		t.Fatal("lost reply cannot be retried", w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restart was not triggered")
	}
	if count.Load() != 1 {
		t.Fatal("queued duplicate restarts")
	}
}

func TestAgentSetupRestartRejectsRevocationAndSupersededSettings(t *testing.T) {
	s, cfg := setupTestServer(t)
	q := savedSetupForRestart(t, s)
	s.deps.Restart = func(context.Context) error { t.Error("refused setup restarted"); return nil }
	checks := 0
	w, _ := agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, func() error {
		checks++
		if checks == 2 {
			return errors.New("revoked")
		}
		return nil
	})
	if w.Code != http.StatusForbidden || checks != 2 {
		t.Fatal(w.Body.String())
	}
	s.deps.CfgMu.Lock()
	cfg.Revision++
	s.deps.CfgMu.Unlock()
	w, _ = agentSetupCall(t, s, protocol.MsgSetupRestart, q, []string{protocol.ScopeSetupWrite}, func() error { return nil })
	if w.Code != http.StatusConflict {
		t.Fatal(w.Body.String())
	}
}

func TestSetupRestartDoesNotClaimARequestWasApplied(t *testing.T) {
	s, _ := setupTestServer(t)
	q := savedSetupForRestart(t, s)
	w, _ := setupCall(t, s, http.MethodPost, "/api/setup/restart", q)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Body.String())
	}
	s.deps.Restart = func(context.Context) error { return errors.New("service unavailable") }
	w, result := setupCall(t, s, http.MethodPost, "/api/setup/restart", q)
	if w.Code != http.StatusAccepted || result["restart_pending"] != true || result["hardware_verified"] != false {
		t.Fatal(w.Body.String())
	}
	// A failed trigger releases the retry guard; it never clears the receipt.
	deadline := time.After(2 * time.Second)
	for {
		s.configWriteMu.Lock()
		queued := s.setupRestartRevision
		s.configWriteMu.Unlock()
		if queued == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("restart failure prevents retry")
		case <-time.After(time.Millisecond):
		}
	}
	w, status := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || status["restart_pending"] != true {
		t.Fatal(w.Body.String())
	}
}
