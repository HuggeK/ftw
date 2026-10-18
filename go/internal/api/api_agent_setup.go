package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type setupAuthorityKey struct{}

type setupAccessError struct {
	status  int
	message string
}

func (e *setupAccessError) Error() string { return e.message }

func setupWriteCheck(r *http.Request) func() error {
	return func() error {
		if r.Context().Err() != nil {
			return &setupAccessError{http.StatusRequestTimeout, "Setup request ended. Read the current status before retrying."}
		}
		if check, ok := r.Context().Value(setupAuthorityKey{}).(func() error); ok {
			if err := check(); err != nil {
				return &setupAccessError{http.StatusForbidden, "Setup access ended. Read the current status after connecting again."}
			}
		}
		// The live authority check may itself wait for another operation.
		if r.Context().Err() != nil {
			return &setupAccessError{http.StatusRequestTimeout, "Setup request ended. Read the current status before retrying."}
		}
		return nil
	}
}

func setupAuthority(w http.ResponseWriter, r *http.Request) bool {
	if err := setupWriteCheck(r)(); err != nil {
		denied := err.(*setupAccessError)
		writeJSON(w, denied.status, map[string]string{"error": denied.Error()})
		return false
	}
	return true
}

// ServeAgentSetup is a typed entry to the same draft service as Settings.
// The caller stays an agent. Its setup grant never opens general API writes,
// grants, Cloud consent, password changes, hardware commands or discovery.
func (s *Server) ServeAgentSetup(w http.ResponseWriter, r *http.Request, message string, call protocol.SetupCall, authorize func() error) {
	caller, named := apiauth.FromRequest(r)
	need, known := protocol.RegistryAgentCalls[message]
	if !known || !named || caller.Kind != apiauth.KindAgent || !apiauth.AgentRole(caller.Role) || !caller.Scopes.Has(need) || authorize == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "This setup operation is not granted."})
		return
	}
	r = r.Clone(context.WithValue(r.Context(), setupAuthorityKey{}, authorize))
	if !setupAuthority(w, r) {
		return
	}
	var path string
	var handle http.HandlerFunc
	switch message {
	case protocol.MsgSetupSchema:
		path, handle = "/api/setup/schema", s.handleSetupSchema
	case protocol.MsgSetupStatus:
		path, handle = "/api/setup/draft", s.handleSetupDraft
	case protocol.MsgSetupEdit:
		path, handle = "/api/setup/draft", s.handleSetupEdit
	case protocol.MsgSetupReset:
		path, handle = "/api/setup/reset", s.handleSetupReset
	case protocol.MsgSetupValidate:
		path, handle = "/api/setup/validate", s.handleSetupValidate
	case protocol.MsgSetupCommit:
		path, handle = "/api/setup/commit", s.handleSetupCommit
	case protocol.MsgSetupRestart:
		path, handle = "/api/setup/restart", s.handleSetupRestart
	}
	r.URL = &url.URL{Path: path}
	r.RequestURI = path
	r.Method = http.MethodPost
	if message == protocol.MsgSetupStatus || message == protocol.MsgSetupSchema {
		r.Method = http.MethodGet
	}
	r.Body = io.NopCloser(bytes.NewReader(call.JSON))
	r.ContentLength = int64(len(call.JSON))
	handle(w, r)
}
