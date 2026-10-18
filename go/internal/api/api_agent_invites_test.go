package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

type inviteAdminStub struct {
	*stubAgents
	created []protocol.AgentInviteCreate
}

func (a *inviteAdminStub) AgentInvites() []protocol.AgentInvite    { return []protocol.AgentInvite{} }
func (a *inviteAdminStub) PrepareAgentInviteHome() (string, error) { return a.home, nil }
func (a *inviteAdminStub) CreateAgentInvite(_ string, m protocol.AgentInviteCreate) (protocol.AgentInvite, error) {
	a.created = append(a.created, m)
	return protocol.AgentInvite{ID: m.ID, Home: m.Home, State: "pending"}, nil
}
func (a *inviteAdminStub) CancelAgentInvite(_, id string) (protocol.AgentInvite, error) {
	return protocol.AgentInvite{ID: id, State: "claimed", AgentID: "claimed1"}, nil
}

func TestInvitationCreationUsesTheSameOwnerGateAndNeverStoresTheBrowserSecret(t *testing.T) {
	secret := bytes.Repeat([]byte{0x72}, 32)
	body, _ := json.Marshal(map[string]any{"id": "request1", "home": "", "secret": base64.StdEncoding.EncodeToString(secret)})
	for _, kind := range []string{"owner", "lan", "box", "session"} {
		t.Run(kind, func(t *testing.T) {
			a := &inviteAdminStub{stubAgents: newStubAgents("abcdefghijklmnopqrstuvwx23")}
			s := New(&Deps{Agents: a})
			r := localRequest(http.MethodPost, "/api/agent-invites", string(body))
			switch kind {
			case "owner":
				r = r.WithContext(withLANSecret(r.Context(), true))
			case "box":
				r = loopbackRequest(http.MethodPost, "/api/agent-invites", string(body))
			case "session":
				r = sessionRequest(http.MethodPost, "/api/agent-invites", string(body))
			}
			w := httptest.NewRecorder()
			s.handleAgentInvites(w, r)
			if kind != "owner" {
				if w.Code != http.StatusForbidden || len(a.created) != 0 {
					t.Fatalf("non-owner created invitation: %d", w.Code)
				}
				return
			}
			if w.Code != 200 || len(a.created) != 1 {
				t.Fatalf("owner request: %d %s", w.Code, w.Body.String())
			}
			if !bytes.Equal(a.created[0].Hash, protocol.AgentInviteHash(a.home, secret)) {
				t.Fatal("wrong home verifier")
			}
			if bytes.Contains(w.Body.Bytes(), []byte(base64.StdEncoding.EncodeToString(secret))) {
				t.Fatal("API echoed bearer secret")
			}
		})
	}
}
