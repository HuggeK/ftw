package main

import (
	"net/http"

	"github.com/srcfl/ftw/go/protocol"
)

// ServeAgentSetup forwards the typed setup entry through the wrapper used
// by real app sessions. The bound API keeps the agent's live authority.
func (l *lateAPI) ServeAgentSetup(w http.ResponseWriter, r *http.Request, message string, call protocol.SetupCall, authorize func() error) {
	srv := l.srv.Load()
	if srv == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"the box is still starting"}`))
		return
	}
	srv.ServeAgentSetup(w, r, message, call, authorize)
}
