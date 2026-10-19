package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/onboarding"
)

type setupOAuthPending struct {
	server                      *Server
	draftRevision, liveRevision int64
	credentialOwner             string
	authorize                   func() error
}

func (s *Server) startSetupMyUplink(r *http.Request, driver config.Driver, revision, base int64, redirect string) (any, error) {
	resolved := config.Config{Drivers: []config.Driver{driver}}
	resolved.ResolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	entry, err := drivers.ParseCatalogFile(resolved.Drivers[0].Lua)
	if err != nil || entry.ID != "myuplink" {
		return nil, errors.New("this driver has no built-in browser sign-in; use its catalogue connection fields in the draft")
	}
	clientID, _ := driver.Config["client_id"].(string)
	clientSecret, _ := driver.Config["client_secret"].(string)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("save this vendor's Client ID and Client Secret in the draft first; do not send tokens to the assistant")
	}
	if !validMyUplinkCallback(redirect) {
		return nil, errors.New("provide the box's browser callback address ending in /api/oauth/myuplink/callback; it must match the vendor registration")
	}
	verifier, challenge, err := newPKCEPair()
	if err != nil {
		return nil, err
	}
	state := make([]byte, 16)
	if _, err := rand.Read(state); err != nil {
		return nil, err
	}
	stateToken := hex.EncodeToString(state)
	scope, _ := driver.Config["oauth_scope"].(string)
	bound := &setupOAuthPending{server: s, draftRevision: revision, liveRevision: base, credentialOwner: driver.CredentialOwner}
	bound.authorize, _ = r.Context().Value(setupAuthorityKey{}).(func() error)
	oauthMu.Lock()
	pruneOAuthStateLocked(time.Now())
	if len(oauthPending_) >= 128 {
		oauthMu.Unlock()
		return nil, errors.New("too many pending vendor sign-ins; finish one or wait for it to expire")
	}
	oauthPending_[stateToken] = oauthPending{driver: driver.Name, redirectURI: redirect, codeVerifier: verifier, expires: time.Now().Add(15 * time.Minute), setup: bound}
	oauthMu.Unlock()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "scope": {myUplinkOAuthScope(scope)}, "state": {stateToken}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	return map[string]any{
		"provider": "myuplink", "authorize_url": myUplinkOAuthBase + "/oauth/authorize?" + q.Encode(), "redirect_uri": redirect,
		"expires_in_s": 900, "draft_revision": revision,
		"manual_completion_path": "/setup-vendor.html",
		"manual_completion_help": "If the registered callback cannot reach this box, open /setup-vendor.html on the local box address you use for FTW, sign in with the house password, then paste the full redirected URL there. Keep that URL out of chat.",
		"next":                   "Open the vendor's sign-in page in your browser. Credentials return only to this exact box draft. Leave the draft unchanged until sign-in finishes; then read setup_status. If it expires or the draft changes, start sign-in again. Never paste access or refresh tokens into chat.",
	}, nil
}

func (s *Server) completeSetupMyUplink(r *http.Request, pending oauthPending, code string) (string, error) {
	bound := pending.setup
	if bound.server != s {
		return "", errors.New("vendor sign-in belongs to another box session")
	}
	live, err := s.setupLive()
	if err != nil {
		return "", err
	}
	if live.Revision != bound.liveRevision {
		return "", onboarding.ErrConflict
	}
	check := func() error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if bound.authorize != nil {
			if err := bound.authorize(); err != nil {
				return errors.New("setup access ended; connect again before signing in")
			}
		}
		current, err := s.setupLive()
		if err != nil {
			return err
		}
		if current.Revision != bound.liveRevision {
			return onboarding.ErrConflict
		}
		return nil
	}
	_, err = s.setup.Prepare(live, bound.draftRevision, func(candidate *config.Config) error {
		for i := range candidate.Drivers {
			driver := &candidate.Drivers[i]
			if driver.Name != pending.driver || driver.CredentialOwner != bound.credentialOwner {
				continue
			}
			clientID, _ := driver.Config["client_id"].(string)
			clientSecret, _ := driver.Config["client_secret"].(string)
			if clientID == "" || clientSecret == "" {
				return errors.New("vendor client credentials are missing from this draft")
			}
			token, err := s.exchangeMyUplinkCode(code, clientID, clientSecret, pending.redirectURI, pending.codeVerifier)
			if err != nil {
				return fmt.Errorf("vendor sign-in failed; start it again: %w", err)
			}
			driver.Config["refresh_token"] = token
			return nil
		}
		return errors.New("the draft's selected driver changed; start sign-in again")
	}, check)
	return pending.driver, err
}
