package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

type claimEnroller struct {
	stubEnroller
	claimed string
}

func (e *claimEnroller) RedeemPairingCode(code, hint string) (string, time.Time, error) {
	e.claimed = code
	if code != "K7M2-9QRT" {
		return "", time.Time{}, ErrBadAppPairingCode
	}
	return "https://app.ftw.energy/p#v3.full-payload", time.Now().Add(time.Minute), nil
}

func claimRequest() *http.Request {
	r := httptest.NewRequest("POST", "http://ftw.local:8080/api/app-link/pairing", strings.NewReader(`{"kind":"claim","code":"K7M2-9QRT","role":"owner"}`))
	r.RemoteAddr = "192.168.1.42:1234"
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestLocalPairingClaimKeepsThePairingSecurityChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		want   int
	}{
		{"local", func(r *http.Request) {}, 200},
		{"public host", func(r *http.Request) { r.Host = "outside.example" }, 403},
		{"remote peer", func(r *http.Request) { r.RemoteAddr = "203.0.113.1:1" }, 403},
		{"proxy", func(r *http.Request) { r.Header.Set("Forwarded", "for=203.0.113.1") }, 403},
		{"cross site", func(r *http.Request) { r.Header.Set("Origin", "https://outside.example") }, 403},
		{"app session", func(r *http.Request) {
			*r = *r.WithContext(apiauth.WithCaller(r.Context(), apiauth.Caller{Kind: apiauth.KindApp, Role: apiauth.RoleOwner, Scopes: apiauth.EveryScope()}))
		}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enroll := &claimEnroller{}
			s := New(&Deps{AppEnroll: enroll})
			r := claimRequest()
			tc.change(r)
			w := httptest.NewRecorder()
			Authenticate(http.HandlerFunc(s.handleAppLinkPairing), MutationPolicy{}).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want != 200 && enroll.claimed != "" {
				t.Fatal("refused request spent the code")
			}
			if enroll.minted != 0 {
				t.Fatal("claim minted the requested role")
			}
		})
	}
}

func TestLocalPairingClaimStillNeedsTheHousePassword(t *testing.T) {
	enroll := &claimEnroller{}
	s := New(&Deps{AppEnroll: enroll})
	policy := MutationPolicy{LANAuthEnabled: func() bool { return true }, VerifyLANSecret: func(secret string) bool { return secret == "house-secret" }}
	h := Authenticate(http.HandlerFunc(s.handleAppLinkPairing), policy)
	for _, token := range []string{"", "house-secret"} {
		r := claimRequest()
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 401
		if token != "" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
	}
}
