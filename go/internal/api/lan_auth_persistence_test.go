package api

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestLANPasswordKeepsExplicitRemoteAPITokenOwner(t *testing.T) {
	resetLANGuesses(t)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal("create local test token failed")
	}
	policy := lanAuthPolicy(testHousePassword)
	policy.RequireTokenForRemote = true
	policy.Token = hex.EncodeToString(raw)
	for _, valid := range []bool{true, false} {
		bearer := policy.Token
		if !valid {
			bearer = "invalid-" + bearer
		}
		req := lanAuthRequest(http.MethodGet, "http://ftw.example.com/api/cars", "203.0.113.1:5123", "Bearer "+bearer)
		var caller apiauth.Caller
		serveLANAuth(policy, req, func(r *http.Request) { caller, _ = apiauth.FromRequest(r) })
		want := apiauth.RoleViewer
		if valid {
			want = apiauth.RoleOwner
		}
		if caller.Role != want {
			t.Fatal("LAN password setting changed explicit API-token authority")
		}
	}
}

func reopenLANServer(t *testing.T, server *Server, path string) *Server {
	t.Helper()
	if err := server.deps.State.Close(); err != nil {
		t.Fatal("close session database failed")
	}
	resetLANSessions() // a new Core process starts without the cache
	st, err := state.Open(path)
	if err != nil {
		t.Fatal("reopen session database failed")
	}
	t.Cleanup(func() { _ = st.Close() })
	deps := *server.deps
	deps.State = st
	deps.SaveConfig = func(path string, cfg *config.Config) error { return config.SaveStored(st, path, cfg) }
	deps.MutationPolicy.VerifyLANSecret = func(secret string) bool { return VerifyStoredLANSecret(st, secret) }
	return New(&deps)
}

func cookieConfigStatus(server *Server, cookie *http.Cookie) int {
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)
	return rr.Code
}

func loginBrowser(t *testing.T, server *Server) *http.Cookie {
	t.Helper()
	rr := postLANAuthJSON(server, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210", `{"password":"`+testHousePassword+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatal("browser login failed")
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == lanSessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("session cookie missing")
	return nil
}

func TestLANSessionSurvivesDatabaseReopen(t *testing.T) {
	resetLANGuesses(t)
	path := filepath.Join(t.TempDir(), "state.db")
	server := newLANAuthServerAt(t, path)
	enableStoredLANAuth(t, server)
	cookie := loginBrowser(t, server)
	server = reopenLANServer(t, server, path)
	if cookieConfigStatus(server, cookie) != http.StatusOK {
		t.Fatal("browser lost its login after a Core restart")
	}
}

func TestLANSessionCookieLastsThirtyDays(t *testing.T) {
	resetLANGuesses(t)
	server := newLANAuthServer(t)
	enableStoredLANAuth(t, server)
	start := time.Now()
	lanSessionMu.Lock()
	lanSessionNow = func() time.Time { return start }
	lanSessionMu.Unlock()
	cookie := loginBrowser(t, server)
	if cookie.MaxAge != 30*24*60*60 || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("browser cookie must last 30 days and retain its protections")
	}
	lanSessionMu.Lock()
	lanSessionNow = func() time.Time { return start.Add(29 * 24 * time.Hour) }
	lanSessionMu.Unlock()
	if cookieConfigStatus(server, cookie) != http.StatusOK {
		t.Fatal("session expired before 30 days")
	}
	lanSessionMu.Lock()
	lanSessionNow = func() time.Time { return start.Add(30 * 24 * time.Hour) }
	lanSessionMu.Unlock()
	if cookieConfigStatus(server, cookie) != http.StatusUnauthorized {
		t.Fatal("expired session remained authorized")
	}
}

func TestLANSessionLogoutStaysRevokedAfterReopen(t *testing.T) {
	resetLANGuesses(t)
	path := filepath.Join(t.TempDir(), "state.db")
	server := newLANAuthServerAt(t, path)
	enableStoredLANAuth(t, server)
	cookie := loginBrowser(t, server)
	req := lanAuthRequest(http.MethodPost, "http://ftw.local:8080/api/auth/logout", "192.168.1.10:43210", "")
	req.Header.Set("Content-Type", "application/json")
	req.Body = http.NoBody
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatal("logout failed")
	}
	server = reopenLANServer(t, server, path)
	if cookieConfigStatus(server, cookie) != http.StatusUnauthorized {
		t.Fatal("logged-out session returned after restart")
	}
}

func TestLANSessionPasswordChangeStaysRevokedAfterReopen(t *testing.T) {
	resetLANGuesses(t)
	path := filepath.Join(t.TempDir(), "state.db")
	server := newLANAuthServerAt(t, path)
	enableStoredLANAuth(t, server)
	cookie := loginBrowser(t, server)
	req := lanAuthRequest(http.MethodPost, "http://ftw.local:8080/api/auth/password", "127.0.0.1:43210", "")
	req.Header.Set("Content-Type", "application/json")
	req.Body = http.NoBody
	req.Body = io.NopCloser(strings.NewReader(`{"enabled":true,"password":"` + testHousePassword + `-changed"}`))
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatal("password change failed")
	}
	server = reopenLANServer(t, server, path)
	if cookieConfigStatus(server, cookie) != http.StatusUnauthorized {
		t.Fatal("old-password session returned after restart")
	}
}
