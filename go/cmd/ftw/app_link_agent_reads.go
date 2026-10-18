package main

import (
	"net/http"

	"github.com/srcfl/ftw/go/protocol"
)

// ServeAgentRead keeps the typed diagnostic entry available through the
// production API wrapper, including after the API binds during startup.
func (l *lateAPI) ServeAgentRead(w http.ResponseWriter, r *http.Request, read protocol.APIRead) {
	srv := l.srv.Load()
	if srv == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"the box is still starting"}`))
		return
	}
	srv.ServeAgentRead(w, r, read)
}
