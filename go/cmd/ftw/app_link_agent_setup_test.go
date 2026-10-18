package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type setupGateway interface {
	ServeAgentSetup(http.ResponseWriter, *http.Request, string, protocol.SetupCall, func() error)
}

var _ setupGateway = (*lateAPI)(nil)

func TestProductionGatewayForwardsTypedSetupAndLiveAuthority(t *testing.T) {
	gateway := &lateAPI{}
	var production setupGateway = gateway
	r := httptest.NewRequest(http.MethodGet, "/api/setup/schema", nil)
	r = r.WithContext(apiauth.WithCaller(r.Context(), apiauth.Caller{Kind: apiauth.KindAgent, Role: apiauth.RoleAgent, Subject: "agent:test", Scopes: apiauth.NewScopeSet(protocol.ScopeDiagRun)}))
	call := protocol.SetupCall{JSON: []byte(`{}`)}
	checks := 0
	authorize := func() error { checks++; return nil }
	starting := httptest.NewRecorder()
	production.ServeAgentSetup(starting, r, protocol.MsgSetupSchema, call, authorize)
	if starting.Code != http.StatusServiceUnavailable {
		t.Fatalf("before bind: %d", starting.Code)
	}
	gateway.bind(api.New(&api.Deps{}))
	answer := httptest.NewRecorder()
	production.ServeAgentSetup(answer, r, protocol.MsgSetupSchema, call, authorize)
	var doc struct {
		Schema map[string]any `json:"schema"`
	}
	if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &doc) != nil || len(doc.Schema) == 0 || checks == 0 {
		t.Fatalf("typed setup did not reach API: %d %s checks=%d", answer.Code, answer.Body.String(), checks)
	}
	revoked := httptest.NewRecorder()
	production.ServeAgentSetup(revoked, r, protocol.MsgSetupSchema, call, func() error { return errors.New("revoked") })
	if revoked.Code != http.StatusForbidden {
		t.Fatalf("live authority lost: %d", revoked.Code)
	}
}
