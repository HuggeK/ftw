package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
)

// The local Settings review computes the same binding as the MCP guide.
// It changes no settings or draft and grants no permission by itself.
func (s *Server) handleSetupConnection(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Fuse config.Fuse `json:"fuse"`
	}
	if !setupJSON(w, r, &q) {
		return
	}
	check := config.NewSetup()
	check.Setup, check.Fuse = nil, q.Fuse
	if err := check.Validate(); err != nil {
		s.setupError(w, err)
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	digest := check.SetupConnectionDigest()
	id, _ := s.setupCommissioning(live)["meter_identity"].(string)
	candidate := *live
	candidate.Fuse = q.Fuse
	candidate.Setup = &config.Setup{ConnectionConfirmed: true, EquipmentReviewed: true, ConnectionDigest: digest, MeterIdentity: id}
	ready, reason := false, "Live equipment checks are unavailable. Keep measurement mode on."
	if s.deps.SetupControlReady != nil {
		if err := s.deps.SetupControlReady(&candidate); err != nil {
			reason = err.Error()
		} else {
			ready, reason = true, ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection_digest": digest, "fuse": q.Fuse, "site_meter": live.SiteMeterDriver(), "meter_identity": id,
		"can_enable_control": ready, "blocked_reason": reason, "saved": false, "hardware_control_verified": false})
}
