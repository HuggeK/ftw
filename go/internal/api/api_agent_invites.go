package api

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"io"
	"net/http"
	"slices"

	"github.com/srcfl/ftw/go/protocol"
)

type AgentInviteAdmin interface {
	PrepareAgentInviteHome() (string, error)
	AgentInvites() []protocol.AgentInvite
	CreateAgentInvite(by string, m protocol.AgentInviteCreate) (protocol.AgentInvite, error)
	CancelAgentInvite(by, id string) (protocol.AgentInvite, error)
}

func (s *Server) agentInviteRoutes() {
	s.handle("GET /api/agent-invites", Local, s.handleAgentInvites)
	s.handle("POST /api/agent-invites", Local, s.handleAgentInvites)
	s.handle("DELETE /api/agent-invites/{id}", Local, s.handleAgentInvites)
}

func (s *Server) handleAgentInvites(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, r.Method == http.MethodPost)
	if !ok {
		return
	}
	admin, ok := s.deps.Agents.(AgentInviteAdmin)
	if !ok {
		writeAppLinkError(w, http.StatusServiceUnavailable, "update FTW to share access")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"invitations": admin.AgentInvites(), "home": s.deps.Agents.Home(), "scopes": apiauth.RoleScopes[apiauth.RoleAgent], "ops": slices.Sorted(slices.Values(inviteOps())), "caps": protocol.Caps{HoldS: protocol.HoldSMax, MaxW: protocol.MaxWMax, PerMin: protocol.PerMinDefault}})
		return
	}
	var v protocol.AgentInvite
	var err error
	if r.Method == http.MethodPost {
		var body struct {
			protocol.AgentInviteCreate
			Secret []byte `json:"secret,omitempty"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			writeAppLinkError(w, http.StatusBadRequest, "invalid invitation fields")
			return
		}
		m := body.AgentInviteCreate
		if len(body.Secret) > 0 {
			if len(body.Secret) != protocol.AgentInviteSecretBytes || len(m.Hash) > 0 {
				writeAppLinkError(w, http.StatusBadRequest, "send one invitation verifier")
				return
			}
			// Reserve even an existing home against concurrent idle teardown.
			// The browser publishes the home returned in the saved receipt.
			var err error
			m.Home, err = admin.PrepareAgentInviteHome()
			if err != nil {
				agentError(w, err)
				return
			}
			m.Hash = protocol.AgentInviteHash(m.Home, body.Secret)
		}
		v, err = admin.CreateAgentInvite(by, m)
	} else {
		v, err = admin.CancelAgentInvite(by, r.PathValue("id"))
	}
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": v})
}

func inviteOps() []string {
	var out []string
	for op := range protocol.RegistryAgentOps {
		out = append(out, op)
	}
	return out
}
