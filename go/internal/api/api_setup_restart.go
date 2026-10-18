package api

import (
	"context"
	"net/http"

	"github.com/srcfl/ftw/go/internal/onboarding"
)

// A setup grant may complete its saved configuration. It does not open the
// general restart route, and cannot restart unrelated Settings changes.
func (s *Server) handleSetupRestart(w http.ResponseWriter, r *http.Request) {
	var q struct {
		SavedRevision int64  `json:"saved_revision"`
		SHA256        string `json:"sha256"`
		Confirmed     bool   `json:"confirmed"`
	}
	if !setupJSON(w, r, &q) {
		return
	}
	if !q.Confirmed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ask the user to confirm the restart and its brief interruption first."})
		return
	}
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	receipt, err := s.setup.Receipt()
	if err != nil {
		s.setupError(w, err)
		return
	}
	if receipt == nil || receipt.SavedRevision != q.SavedRevision || receipt.SHA256 != q.SHA256 || live.Revision != q.SavedRevision {
		s.setupError(w, onboarding.ErrConflict)
		return
	}
	if !setupAuthority(w, r) {
		return
	}
	if !setupRestartPending(receipt) {
		writeJSON(w, http.StatusOK, map[string]any{"restart_requested": false, "restart_pending": false, "saved_revision": q.SavedRevision, "hardware_verified": false})
		return
	}
	if s.deps.Restart == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Restart is unavailable here. The setup is saved and still needs a process restart."})
		return
	}
	if s.setupRestartRevision != q.SavedRevision {
		s.setupRestartRevision = q.SavedRevision
		s.queueRestart(func(ctx context.Context) error {
			s.configWriteMu.Lock()
			defer s.configWriteMu.Unlock()
			if authorize, ok := r.Context().Value(setupAuthorityKey{}).(func() error); ok {
				if err := authorize(); err != nil {
					return err
				}
			}
			current, err := s.setupLive()
			if err != nil {
				return err
			}
			receipt, err := s.setup.Receipt()
			if err != nil {
				return err
			}
			if current.Revision != q.SavedRevision || receipt == nil || receipt.SavedRevision != q.SavedRevision || receipt.SHA256 != q.SHA256 {
				return onboarding.ErrConflict
			}
			return s.deps.Restart(ctx)
		}, func(err error) {
			if err != nil {
				s.configWriteMu.Lock()
				if s.setupRestartRevision == q.SavedRevision {
					s.setupRestartRevision = 0
				}
				s.configWriteMu.Unlock()
			}
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"restart_requested": true, "restart_pending": true, "saved_revision": q.SavedRevision, "hardware_verified": false, "next_step": "The connection will pause. Reconnect and read setup_status to verify that the saved configuration is applied; a restart request alone does not prove it."})
}
