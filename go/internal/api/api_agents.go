package api

// Agents on the box's own page (docs/agents.md, Approval on the box page).
//
// An owner phone manages agents over its session, each grant signed by its
// approval key. On the home network the box is itself the authority, so
// these routes ask for no signature. They ask for something else: widening
// access needs the box itself or the house password. With api.lan_auth off,
// which is the default, every device on the home network is an owner with no
// sign-in at all, and granting a remote agent from there would let any of
// them open the house to the outside. Narrowing stays open to any LAN
// client, as DELETE /api/cloud/grant does.
//
// Every route is local: the session passthrough never carries them, so a
// phone must use its signed messages and an agent can never reach them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/srcfl/ftw/go/protocol"
)

// AgentAdmin is the box page's view of agent grants. by names the caller
// for the audit log: "box" for the box itself, or "lan:<address>".
type AgentAdmin interface {
	Agents() []protocol.AgentRow
	Requests() []protocol.Request
	ApprovalKeys() []protocol.ApprovalKeyInfo
	// Home is this box's home id at FTW Cloud, or "" before it links.
	Home() string
	// Wall is the wall clock in ms, and whether NTP has set it.
	Wall() (int64, bool)
	// OpenOffer checks a signed offer, base64url as the consent page gave
	// it, against the offer keys this build pins.
	OpenOffer(raw string) (protocol.GrantOffer, error)
	// ReplaceApprovalKey sets an owner phone's approval key, for a phone
	// that lost its own.
	ReplaceApprovalKey(by, deviceID string, pub []byte) error
	AddAgent(by string, m protocol.AgentAdd) (protocol.AgentRow, error)
	WidenAgent(by string, m protocol.AgentWiden) (protocol.AgentRow, error)
	RevokeAgent(by, id string) error
	Decide(ctx context.Context, by, reqID string, approve bool) (string, *protocol.CmdResult, error)
	Audit(beforeSeq int64, n int) ([]protocol.AuditRow, bool, error)
	// ResetClock makes the box trust its clock again after it once ran
	// ahead, and ends every agent's grant.
	ResetClock(by string) error
}

func (s *Server) agentRoutes() {
	s.handle("GET    /api/agents", Local, s.handleAgentsList)
	s.handle("GET    /api/agents/audit", Local, s.handleAgentsAudit)
	s.handle("POST   /api/agents", Local, s.handleAgentsAdd)
	s.handle("POST   /api/agents/{id}/widen", Local, s.handleAgentsWiden)
	s.handle("DELETE /api/agents/{id}", Local, s.handleAgentsRevoke)
	s.handle("POST   /api/agent-requests/{reqId}", Local, s.handleAgentsDecide)
	s.handle("POST   /api/agents/approval-key", Local, s.handleAgentsApprovalKey)
	s.handle("POST   /api/agents/clock-reset", Local, s.handleAgentsClockReset)
}

// handleAgentsClockReset is for a clock that once ran ahead and now keeps
// every agent locked out. It needs the house password, and every agent's
// grant ends.
func (s *Server) handleAgentsClockReset(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	if err := s.deps.Agents.ResetClock(by); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentsGate admits a client on the home network, and with widen only one
// signed in with the house password, from any address: a driver's Lua and
// the session passthrough can both reach the box from inside it, so loopback
// proves nothing. It returns the caller's name for the audit log.
func (s *Server) agentsGate(w http.ResponseWriter, r *http.Request, widen bool) (string, bool) {
	if s.deps == nil || s.deps.Agents == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "agents are not available on this box")
		return "", false
	}
	auth, err := parseAuthority(r.Host)
	if hasForwardingHeader(r.Header) || appLinkOverSession(r) || err != nil ||
		!isLocalAuthority(auth) || !isLocalClient(r.RemoteAddr) {
		writeAppLinkError(w, http.StatusForbidden, "agents are managed in the app or on your home network")
		return "", false
	}
	by := "lan:" + remoteHost(r)
	if isLoopbackClient(r.RemoteAddr) {
		by = "box"
	}
	ok, checked := lanSecretFrom(r.Context())
	if checked && !ok {
		// A wrong house password is a refusal, even for a route that only
		// narrows.
		writeAppLinkError(w, http.StatusForbidden, "sign in with the house password")
		return "", false
	}
	if !widen || ok {
		return by, true
	}
	writeAppLinkError(w, http.StatusForbidden,
		"give access in the app, or on your home network signed in with the house password")
	return "", false
}

// agentError answers a refusal with its code, and anything else as a 500.
func agentError(w http.ResponseWriter, err error) {
	var r *protocol.Refusal
	if !errors.As(err, &r) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusBadRequest
	switch r.Code {
	case protocol.ErrNotFound:
		status = http.StatusNotFound
	case protocol.ErrConflict:
		status = http.StatusConflict
	case protocol.ErrUnavailable:
		status = http.StatusServiceUnavailable
	case protocol.ErrScopeDenied:
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]any{"error": r.Code, "args": r.Args})
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentsGate(w, r, false); !ok {
		return
	}
	a := s.deps.Agents
	writeJSON(w, http.StatusOK, protocol.AgentList{
		Agents: nonNil(a.Agents()), Requests: nonNil(a.Requests()), Keys: nonNil(a.ApprovalKeys()),
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) handleAgentsAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentsGate(w, r, false); !ok {
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	rows, more, err := s.deps.Agents.Audit(before, n)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.AuditPage{Rows: nonNil(rows), More: more})
}

// agentAddBody is a grant offer, as FTW Cloud's consent page shows it, and
// what the owner chose to grant of it. A field left out grants what the
// offer asks.
type agentAddBody struct {
	Offer  string         `json:"offer"`
	Scopes []string       `json:"scopes,omitempty"`
	Ops    []string       `json:"ops,omitempty"`
	Caps   *protocol.Caps `json:"caps,omitempty"`
	TTLS   uint64         `json:"ttlS,omitempty"`
}

func (s *Server) handleAgentsAdd(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body agentAddBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not a grant"})
		return
	}
	offer, err := s.deps.Agents.OpenOffer(body.Offer)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offer is not signed, has expired or is not readable"})
		return
	}
	if home := s.deps.Agents.Home(); offer.Home != "" && offer.Home != home {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offer is for another box"})
		return
	}
	now, synced := s.deps.Agents.Wall()
	if !synced {
		agentError(w, protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"}))
		return
	}
	if !withinOffer(offer, body) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the box may grant less than the offer, not more"})
		return
	}
	m := protocol.AgentAdd{
		Key: offer.Key, Role: offer.Role, Scopes: offer.Scopes, Ops: offer.Ops, Caps: offer.Caps,
		Principal: protocol.Principal{Client: offer.Client, ClientID: offer.ClientID, Ticket: offer.Ticket},
	}
	if body.Scopes != nil {
		m.Scopes = body.Scopes
	}
	if body.Ops != nil {
		m.Ops = body.Ops
	}
	if body.Caps != nil {
		m.Caps = *body.Caps
	}
	ttl := offer.TTLS
	if body.TTLS != 0 {
		ttl = body.TTLS
	}
	m.ExpiresAtMs = now + int64(ttl)*int64(time.Second/time.Millisecond)
	row, err := s.deps.Agents.AddAgent(by, m)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

// withinOffer reports whether what the owner chose on the page gives no more
// than the offer the agent's client signed for.
func withinOffer(offer protocol.GrantOffer, body agentAddBody) bool {
	for _, scope := range body.Scopes {
		if !slices.Contains(offer.Scopes, scope) {
			return false
		}
	}
	for _, op := range body.Ops {
		if !slices.Contains(offer.Ops, op) {
			return false
		}
	}
	if c := body.Caps; c != nil {
		perMin := func(n uint64) uint64 {
			if n == 0 {
				return protocol.PerMinDefault
			}
			return n
		}
		if c.HoldS > offer.Caps.HoldS || c.MaxW > offer.Caps.MaxW || perMin(c.PerMin) > perMin(offer.Caps.PerMin) {
			return false
		}
	}
	return body.TTLS <= offer.TTLS
}

// agentWidenBody is a grant's new rights, whole.
type agentWidenBody struct {
	Epoch  uint64        `json:"epoch"`
	Scopes []string      `json:"scopes"`
	Ops    []string      `json:"ops"`
	Caps   protocol.Caps `json:"caps"`
	TTLS   uint64        `json:"ttlS"`
}

func (s *Server) handleAgentsWiden(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body agentWidenBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not a grant"})
		return
	}
	now, synced := s.deps.Agents.Wall()
	if !synced {
		agentError(w, protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"}))
		return
	}
	row, err := s.deps.Agents.WidenAgent(by, protocol.AgentWiden{
		ID: r.PathValue("id"), Epoch: body.Epoch, Scopes: body.Scopes, Ops: body.Ops, Caps: body.Caps,
		ExpiresAtMs: now + int64(body.TTLS)*int64(time.Second/time.Millisecond),
	})
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleAgentsRevoke(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, false)
	if !ok {
		return
	}
	if err := s.deps.Agents.RevokeAgent(by, r.PathValue("id")); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentsDecide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve bool `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body needs approve"})
		return
	}
	// Approving needs what widening needs; declining is open, as removing
	// access is.
	by, ok := s.agentsGate(w, r, body.Approve)
	if !ok {
		return
	}
	state, res, err := s.deps.Agents.Decide(r.Context(), by, r.PathValue("reqId"), body.Approve)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "result": res})
}

// handleAgentsApprovalKey sets an owner phone's approval key from the box
// page, for a phone whose key a new Face ID enrolment voided.
func (s *Server) handleAgentsApprovalKey(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Pub      string `json:"pub"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body needs deviceId and pub"})
		return
	}
	pub, err := base64.RawURLEncoding.DecodeString(body.Pub)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub is not base64url"})
		return
	}
	if err := s.deps.Agents.ReplaceApprovalKey(by, body.DeviceID, pub); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
