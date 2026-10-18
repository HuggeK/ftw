package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type diagnosticGateway interface {
	ServeAgentRead(http.ResponseWriter, *http.Request, protocol.APIRead)
}

var _ diagnosticGateway = (*lateAPI)(nil)

func TestProductionGatewayForwardsTypedDiagnosticsAfterBinding(t *testing.T) {
	gateway := &lateAPI{}
	var production diagnosticGateway = gateway
	request := httptest.NewRequest(http.MethodGet, protocol.AgentAPICatalogPath, nil)
	request = request.WithContext(apiauth.WithCaller(request.Context(), apiauth.Caller{
		Kind: apiauth.KindAgent, Role: apiauth.RoleAgent, Subject: "agent:test",
		Scopes: apiauth.NewScopeSet(protocol.ScopeDiagRun),
	}))
	starting := httptest.NewRecorder()
	production.ServeAgentRead(starting, request, protocol.APIRead{})
	if starting.Code != http.StatusServiceUnavailable {
		t.Fatalf("before bind: %d", starting.Code)
	}
	gateway.bind(api.New(&api.Deps{}))
	answer := httptest.NewRecorder()
	production.ServeAgentRead(answer, request, protocol.APIRead{})
	var catalog struct {
		Routes        []any  `json:"routes"`
		RequiredScope string `json:"requiredScope"`
	}
	if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &catalog) != nil || len(catalog.Routes) == 0 || catalog.RequiredScope != protocol.ScopeDiagRun {
		t.Fatalf("production diagnostics did not reach typed API: %d %s", answer.Code, answer.Body.String())
	}
	denied := httptest.NewRecorder()
	production.ServeAgentRead(denied, httptest.NewRequest(http.MethodGet, protocol.AgentAPICatalogPath, nil), protocol.APIRead{})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing grant bypassed: %d", denied.Code)
	}
}
