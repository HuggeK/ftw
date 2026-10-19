package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

func TestSetupVendorConsentStaysWithTheExactDraftAndGrant(t *testing.T) {
	var exchanges atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_secret") != "draft-client-secret" || r.Form.Get("code_verifier") == "" {
			t.Error("token exchange lost its draft credentials or PKCE")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refresh_token":"vendor-refresh-secret"}`))
	}))
	defer vendor.Close()
	old := myUplinkOAuthBase
	myUplinkOAuthBase = vendor.URL
	defer func() { myUplinkOAuthBase = old }()
	for _, scenario := range []string{"success", "draft_changed", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			s, live := setupTestServer(t)
			file := filepath.Join(t.TempDir(), "myuplink.lua")
			if err := os.WriteFile(file, []byte("DRIVER = {\n id=\"myuplink\",\n config_secrets={\"client_secret\", \"refresh_token\"}\n}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s.deps.UserDriverDir = filepath.Dir(file)
			w, draft := setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": []any{map[string]any{
				"name": "heat", "lua": file, "capabilities": map[string]any{"standalone": true}, "config": map[string]any{"client_id": "draft-client", "client_secret": "draft-client-secret"},
			}}}})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			revoked := false
			guard := func() error {
				if revoked {
					return errors.New("revoked")
				}
				return nil
			}
			w, started := agentSetupCall(t, s, protocol.MsgSetupEdit, map[string]any{
				"revision": draft["revision"], "action": "connect", "driver": "heat", "confirmed": true,
				"redirect_uri": "http://box.local:8080/api/oauth/myuplink/callback",
			}, []string{protocol.ScopeSetupWrite}, guard)
			if w.Code != 200 || strings.Contains(w.Body.String(), "draft-client-secret") {
				t.Fatal(w.Body.String())
			}

			if started["result"].(map[string]any)["manual_completion_path"] != "/setup-vendor.html" {
				t.Fatal("fallback must use owner's local box address")
			}
			link, err := url.Parse(started["result"].(map[string]any)["authorize_url"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "draft_changed" {
				w, _ = setupCall(t, s, "POST", "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"site": map[string]any{"name": "Changed while signing in"}}})
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			revoked = scenario == "revoked"
			beforeExchanges := exchanges.Load()
			callback := httptest.NewRequest("GET", "http://box.local:8080/api/oauth/myuplink/callback?code=one-time-code&state="+link.Query().Get("state"), nil)
			reply := httptest.NewRecorder()
			s.Handler().ServeHTTP(reply, callback)
			stored, err := s.setup.Read(live)
			if err != nil {
				t.Fatal(err)
			}
			if live.EVCharger.Password != "before-secret" || strings.Contains(reply.Body.String(), "vendor-refresh-secret") {
				t.Fatal("consent changed live config or returned a token")
			}
			if scenario == "success" {
				if reply.Code != 200 || !strings.Contains(string(stored.Config), "vendor-refresh-secret") || stored.Revision != 2 || !strings.Contains(reply.Body.String(), "saved to setup draft") {
					t.Fatal(reply.Code, reply.Body.String(), stored.Revision)
				}
			} else if reply.Code < 400 || exchanges.Load() != beforeExchanges || strings.Contains(string(stored.Config), "vendor-refresh-secret") {
				t.Fatal("stale or revoked consent reached the vendor or changed a draft", reply.Code, reply.Body.String())
			}
		})
	}
}
