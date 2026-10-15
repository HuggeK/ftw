package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/protocol"
)

type stubAgents struct {
	keys     map[string][]byte
	replaced []string
	added    []protocol.AgentAdd
	revoked  []string
	decided  []bool
	by       []string
	home     string
	unsynced bool
	resets   int
}

func (a *stubAgents) Agents() []protocol.AgentRow              { return nil }
func (a *stubAgents) Requests() []protocol.Request             { return nil }
func (a *stubAgents) ApprovalKeys() []protocol.ApprovalKeyInfo { return nil }
func (a *stubAgents) Home() string                             { return a.home }
func (a *stubAgents) Wall() (int64, bool)                      { return 1_000_000, !a.unsynced }
func (a *stubAgents) AddAgent(by string, m protocol.AgentAdd) (protocol.AgentRow, error) {
	a.added = append(a.added, m)
	a.by = append(a.by, by)
	return protocol.AgentRow{ID: "agent001", Role: m.Role}, nil
}
func (a *stubAgents) WidenAgent(by string, m protocol.AgentWiden) (protocol.AgentRow, error) {
	a.by = append(a.by, by)
	return protocol.AgentRow{ID: m.ID}, nil
}
func (a *stubAgents) RevokeAgent(by, id string) error {
	a.revoked = append(a.revoked, id)
	a.by = append(a.by, by)
	return nil
}
func (a *stubAgents) Decide(_ context.Context, by, _ string, approve bool) (string, *protocol.CmdResult, error) {
	a.decided = append(a.decided, approve)
	a.by = append(a.by, by)
	return "approved", nil, nil
}
func (a *stubAgents) Audit(int64, int) ([]protocol.AuditRow, bool, error) { return nil, false, nil }
func (a *stubAgents) ResetClock(by string) error {
	a.resets++
	a.by = append(a.by, by)
	return nil
}
func (a *stubAgents) OpenOffer(raw string) (protocol.GrantOffer, error) {
	signed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return protocol.GrantOffer{}, err
	}
	var so protocol.SignedOffer
	if err := protocol.UnmarshalSigned(signed, &so); err != nil {
		return protocol.GrantOffer{}, err
	}
	return protocol.OpenOffer(a.keys, so, 1_000_000)
}
func (a *stubAgents) ReplaceApprovalKey(by, deviceID string, _ []byte) error {
	a.replaced = append(a.replaced, deviceID)
	a.by = append(a.by, by)
	return nil
}

// offerSigner is FTW Cloud's offer key, for the tests.
var offerSigner = approvaltest.New()

func newStubAgents(home string) *stubAgents {
	return &stubAgents{home: home, keys: map[string][]byte{"k1": offerSigner.Pub()}}
}

func offerBody(t *testing.T, home string) string {
	t.Helper()
	return signedOfferBody(t, home, offerSigner)
}

func signedOfferBody(t *testing.T, home string, signer *approvaltest.Signer) string {
	t.Helper()
	o, err := protocol.Marshal(protocol.GrantOffer{
		V: protocol.GrantOfferVersion, Home: home, Key: make([]byte, 32), Role: apiauth.RoleAgent,
		Client: "Claude", Scopes: []string{protocol.ScopeLiveRead}, Ops: []string{}, TTLS: 3600, Code: "ABCD",
		ExpMs: 1_000_000 + 60_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.OfferBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.Marshal(protocol.SignedOffer{O: o, Kid: "k1", Sig: signer.Sign(msg)})
	if err != nil {
		t.Fatal(err)
	}
	return `{"offer":"` + base64.RawURLEncoding.EncodeToString(raw) + `"}`
}

// Giving an agent access from the box page needs the house password, from
// any address: with lan_auth off every device on the Wi-Fi is an owner with
// no sign-in, and a driver or the passthrough can reach the box from inside.
func TestWhoMayGiveAnAgentAccessOnTheBoxPage(t *testing.T) {
	body := offerBody(t, "home-1")
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"the box itself, without the password", loopbackRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"a LAN client", localRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"a LAN client with the house password", func() *http.Request {
			r := localRequest(http.MethodPost, "/api/agents", body)
			return r.WithContext(withLANSecret(r.Context(), true))
		}(), http.StatusCreated},
		{"an owner's phone through the passthrough", sessionRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"through a proxy", func() *http.Request {
			r := loopbackRequest(http.MethodPost, "/api/agents", body)
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			return r
		}(), http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agents := newStubAgents("home-1")
			s := New(&Deps{Agents: agents})
			w := httptest.NewRecorder()
			s.handleAgentsAdd(w, c.req)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if added := len(agents.added) == 1; added != (c.want == http.StatusCreated) {
				t.Fatalf("added = %v with status %d", added, w.Code)
			}
		})
	}
}

func signedIn(r *http.Request) *http.Request { return r.WithContext(withLANSecret(r.Context(), true)) }

func TestTheBoxPageTakesASignedOfferAndItsLength(t *testing.T) {
	agents := newStubAgents("home-1")
	s := New(&Deps{Agents: agents})
	add := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleAgentsAdd(w, signedIn(localRequest(http.MethodPost, "/api/agents", body)))
		return w
	}
	if w := add(offerBody(t, "")); w.Code != http.StatusCreated || len(agents.added) != 1 {
		t.Fatalf("an offer that leaves the home to the owner: %d %s", w.Code, w.Body.String())
	}
	m := agents.added[0]
	if m.ExpiresAtMs != 1_000_000+3_600_000 || m.Principal.Client != "Claude" || agents.by[0] != "lan:192.168.1.5" {
		t.Errorf("grant %+v by %v", m, agents.by)
	}
	if w := add(offerBody(t, "home-1")); w.Code != http.StatusCreated {
		t.Errorf("an offer for this box: %d", w.Code)
	}
	if w := add(offerBody(t, "another-home")); w.Code != http.StatusBadRequest {
		t.Errorf("an offer for another box: %d", w.Code)
	}
	if w := add(signedOfferBody(t, "", approvaltest.New())); w.Code != http.StatusBadRequest {
		t.Errorf("an offer no pinned key signed: %d", w.Code)
	}
	more := func(extra string) string {
		b := offerBody(t, "home-1")
		return b[:len(b)-1] + "," + extra + "}"
	}
	for _, extra := range []string{`"scopes":["ftw.logs.read"]`, `"ops":["loadpoint.hold"]`, `"caps":{"holdS":60}`, `"ttlS":7200`} {
		if w := add(more(extra)); w.Code != http.StatusBadRequest {
			t.Errorf("more than the offer, %s: %d", extra, w.Code)
		}
	}
	if w := add(more(`"scopes":[],"ttlS":60`)); w.Code != http.StatusCreated {
		t.Errorf("less than the offer: %d %s", w.Code, w.Body.String())
	}
	agents.unsynced = true
	if w := add(offerBody(t, "home-1")); w.Code != http.StatusServiceUnavailable {
		t.Errorf("without a clock: %d", w.Code)
	}
}

func TestALostApprovalKeyIsReplacedOnlyWithThePassword(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})
	body := `{"deviceId":"ownerPh1","pub":"` + base64.RawURLEncoding.EncodeToString(approvaltest.New().Pub()) + `"}`
	w := httptest.NewRecorder()
	s.handleAgentsApprovalKey(w, localRequest(http.MethodPost, "/api/agents/approval-key", body))
	if w.Code != http.StatusForbidden || len(agents.replaced) != 0 {
		t.Fatalf("without the password: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleAgentsApprovalKey(w, signedIn(localRequest(http.MethodPost, "/api/agents/approval-key", body)))
	if w.Code != http.StatusNoContent || len(agents.replaced) != 1 {
		t.Fatalf("with the password: %d %s", w.Code, w.Body.String())
	}
}

// Resetting the clock's mark ends every grant and needs the house password.
func TestTheClockResetNeedsThePassword(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})
	w := httptest.NewRecorder()
	s.handleAgentsClockReset(w, localRequest(http.MethodPost, "/api/agents/clock-reset", ""))
	if w.Code != http.StatusForbidden || agents.resets != 0 {
		t.Fatalf("without the password: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleAgentsClockReset(w, signedIn(localRequest(http.MethodPost, "/api/agents/clock-reset", "")))
	if w.Code != http.StatusNoContent || agents.resets != 1 {
		t.Fatalf("with the password: %d %s", w.Code, w.Body.String())
	}
}

// Removing access and declining a request narrow it, so any LAN client may.
// Approving a request widens what an agent did, so it needs the password.
func TestNarrowingIsOpenOnTheLANAndApprovingIsNot(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})

	w := httptest.NewRecorder()
	r := localRequest(http.MethodDelete, "/api/agents/agent001", "")
	r.SetPathValue("id", "agent001")
	s.handleAgentsRevoke(w, r)
	if w.Code != http.StatusNoContent || len(agents.revoked) != 1 || agents.by[0] != "lan:192.168.1.5" {
		t.Fatalf("revoke: %d %v %v", w.Code, agents.revoked, agents.by)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":false}`)
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusOK || len(agents.decided) != 1 {
		t.Fatalf("decline: %d", w.Code)
	}

	// A wrong house password is refused even where none is needed.
	w = httptest.NewRecorder()
	r = localRequest(http.MethodDelete, "/api/agents/agent002", "")
	r = r.WithContext(withLANSecret(r.Context(), false))
	r.SetPathValue("id", "agent002")
	s.handleAgentsRevoke(w, r)
	if w.Code != http.StatusForbidden || len(agents.revoked) != 1 {
		t.Fatalf("revoke with a wrong password: %d %v", w.Code, agents.revoked)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":true}`)
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusForbidden || len(agents.decided) != 1 {
		t.Fatalf("approve without the password: %d", w.Code)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":true}`)
	r = r.WithContext(withLANSecret(r.Context(), true))
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusOK || len(agents.decided) != 2 || !agents.decided[1] {
		t.Fatalf("approve with the password: %d", w.Code)
	}
}

// Every agent route is local: the passthrough never carries one.
func TestAgentRoutesAreLocal(t *testing.T) {
	s := New(&Deps{Agents: newStubAgents("")})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/agents"},
		{http.MethodGet, "/api/agents/audit"},
		{http.MethodPost, "/api/agents"},
		{http.MethodPost, "/api/agents/abc/widen"},
		{http.MethodDelete, "/api/agents/abc"},
		{http.MethodPost, "/api/agent-requests/r1"},
		{http.MethodPost, "/api/agents/approval-key"},
	} {
		if facts := s.Route(httptest.NewRequest(c.method, c.path, nil)); facts.Tier != apiauth.TierLocal {
			t.Errorf("%s %s is %q", c.method, c.path, facts.Tier)
		}
	}
}

// With the house password on, a LAN client that has not signed in reads
// neither the agents nor their audit log, and changes nothing.
func TestTheAgentRoutesNeedSignInWithLANAuthOn(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/agents"},
		{http.MethodGet, "/api/agents/audit"},
		{http.MethodDelete, "/api/agents/agent001"},
		{http.MethodPost, "/api/agent-requests/r1"},
	} {
		if rr := serveLANAuth(policy, lanAuthRequest(c.method, "http://ftw.local:8080"+c.path, "192.168.1.5:1234", ""), nil); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without sign-in: %d", c.method, c.path, rr.Code)
		}
	}
	if rr := serveLANAuth(policy, lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/agents", "192.168.1.5:1234", "Bearer "+testHousePassword), nil); rr.Code != http.StatusNoContent {
		t.Errorf("signed in: %d", rr.Code)
	}
}
