package appproto

import (
	"net/http"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type diagnosticGateway struct {
	stubAPI
	read func(http.ResponseWriter, *http.Request)
}

func (g *diagnosticGateway) ServeAgentRead(w http.ResponseWriter, r *http.Request, _ protocol.APIRead) {
	g.read(w, r)
}

func TestAnAgentDiagnosticReadNeedsOnlyItsDiagnosticGrant(t *testing.T) {
	for _, hasDiagnostic := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing diagnostic grant", true: "granted"}[hasDiagnostic], func(t *testing.T) {
			r := newAgentRig(t)
			scopes := []string{protocol.ScopeHistoryRead}
			if hasDiagnostic {
				scopes = []string{protocol.ScopeDiagRun}
			}
			row, _ := r.grant(apiauth.RoleAgent, scopes, nil, protocol.Caps{}, time.Hour, protocol.Principal{Client: "ChatGPT"})
			h, rec := r.agentSession(row)
			t.Cleanup(h.Close)
			called := make(chan struct{}, 1)
			h.cfg.API = &diagnosticGateway{stubAPI: stubAPI{facts: apiauth.RouteFacts{Tier: apiauth.TierRead}}, read: func(w http.ResponseWriter, request *http.Request) {
				called <- struct{}{}
				if request.Method != http.MethodGet || request.URL.Path != "/api/energy/history" {
					t.Error("wrong diagnostic request")
				}
				_, _ = w.Write([]byte(`{"energy_wh":12}`))
			}}
			deliver(t, h, protocol.MsgAPIRead, ptrU32(1), protocol.APIRead{Path: "/api/energy/history"})
			if !hasDiagnostic {
				if got := body[ErrorBody](t, waitFor(t, rec, MsgError)); got.Code != ErrScopeDenied {
					t.Fatalf("missing scope: %+v", got)
				}
				select {
				case <-called:
					t.Fatal("a denied read reached its handler")
				default:
				}
				return
			}
			end := body[APIEnd](t, waitFor(t, rec, MsgAPIEnd))
			if end.Truncated || string(apiChunks(rec)) != `{"energy_wh":12}` {
				t.Fatalf("diagnostic stream: %+v %s", end, apiChunks(rec))
			}
		})
	}
}

func TestRevocationStopsADiagnosticReadBeforeItsAnswer(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeDiagRun, protocol.ScopeHistoryRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	t.Cleanup(h.Close)
	started, release := make(chan struct{}), make(chan struct{})
	h.cfg.API = &diagnosticGateway{stubAPI: stubAPI{facts: apiauth.RouteFacts{Tier: apiauth.TierRead}}, read: func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"must_not_leave":123}`))
	}}
	deliver(t, h, protocol.MsgAPIRead, ptrU32(1), protocol.APIRead{Path: "/api/energy/history"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("diagnostic handler did not start")
	}
	if err := r.access.RevokeAgent(row.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := body[ErrorBody](t, waitFor(t, rec, MsgError)); got.Code != ErrGrantRevoked {
		t.Fatalf("revoked read: %+v", got)
	}
	if rec.has(MsgAPIHead) || len(apiChunks(rec)) != 0 {
		t.Fatal("a revoked read returned data")
	}
}
