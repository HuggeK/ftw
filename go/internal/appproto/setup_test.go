package appproto

import (
	"net/http"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type setupTestGateway struct {
	stubAPI
	call func(http.ResponseWriter, *http.Request, string, protocol.SetupCall, func() error)
}

func (g *setupTestGateway) ServeAgentSetup(w http.ResponseWriter, r *http.Request, message string, call protocol.SetupCall, guard func() error) {
	g.call(w, r, message, call, guard)
}

func TestSetupCallUsesTheAgentsOwnNoiseSessionAndGrant(t *testing.T) {
	for _, scope := range []string{protocol.ScopeDiagRun, protocol.ScopeSetupWrite} {
		t.Run(scope, func(t *testing.T) {
			r := newAgentRig(t)
			row, _ := r.grant(apiauth.RoleAgent, []string{scope}, nil, protocol.Caps{}, time.Hour, protocol.Principal{Client: "ChatGPT"})
			h, rec := r.agentSession(row)
			t.Cleanup(h.Close)
			called := make(chan struct{}, 1)
			h.cfg.API = &setupTestGateway{call: func(w http.ResponseWriter, req *http.Request, message string, call protocol.SetupCall, guard func() error) {
				called <- struct{}{}
				caller, ok := apiauth.FromRequest(req)
				if !ok || caller.Kind != apiauth.KindAgent || caller.Role != apiauth.RoleAgent || !caller.Scopes.Has(protocol.ScopeSetupWrite) {
					t.Error("caller identity changed")
				}
				if message != protocol.MsgSetupEdit || string(call.JSON) != `{"revision":0,"patch":{"site":{"name":"Home"}}}` || guard() != nil {
					t.Error("request changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"revision":1,"saved":false}`))
			}}
			deliver(t, h, protocol.MsgSetupEdit, ptrU32(1), protocol.SetupCall{JSON: []byte(`{"revision":0,"patch":{"site":{"name":"Home"}}}`)})
			if scope == protocol.ScopeDiagRun {
				if got := body[ErrorBody](t, waitFor(t, rec, MsgError)); got.Code != ErrScopeDenied {
					t.Fatal(got)
				}
				select {
				case <-called:
					t.Fatal("read grant reached a write")
				default:
				}
				return
			}
			end := body[APIEnd](t, waitFor(t, rec, MsgAPIEnd))
			if end.Truncated || string(apiChunks(rec)) != `{"revision":1,"saved":false}` {
				t.Fatal(end, string(apiChunks(rec)))
			}
		})
	}
}

func TestRevocationReachesTheSetupSaveGuard(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeSetupWrite}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	t.Cleanup(h.Close)
	started, release, checked := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	h.cfg.API = &setupTestGateway{call: func(w http.ResponseWriter, _ *http.Request, _ string, _ protocol.SetupCall, guard func() error) {
		close(started)
		<-release
		checked <- guard()
		_, _ = w.Write([]byte(`{"must_not_leave":true}`))
	}}
	deliver(t, h, protocol.MsgSetupCommit, ptrU32(1), protocol.SetupCall{JSON: []byte(`{}`)})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("setup not called")
	}
	if err := r.access.RevokeAgent(row.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-checked:
		if err == nil {
			t.Fatal("revocation did not reach save guard")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard not checked")
	}
	if got := body[ErrorBody](t, waitFor(t, rec, MsgError)); got.Code != ErrGrantRevoked {
		t.Fatal(got)
	}
	if rec.has(MsgAPIHead) || len(apiChunks(rec)) > 0 {
		t.Fatal("revoked setup returned an answer")
	}
}
