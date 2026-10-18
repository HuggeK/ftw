package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/onboarding"
)

func (s *Server) setupRoutes() {
	s.handle("GET /api/setup/schema", Read, s.handleSetupSchema)
	s.handle("GET /api/setup/draft", Local, s.handleSetupDraft)
	s.handle("POST /api/setup/draft", Configure, s.handleSetupEdit)
	s.handle("POST /api/setup/restart", Configure, s.handleSetupRestart)
	s.handle("POST /api/setup/validate", Configure, s.handleSetupValidate)
	s.handle("POST /api/setup/commit", Configure, s.handleSetupCommit)
}

func (s *Server) handleSetupSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schema": onboarding.Schema(), "drivers": "/api/drivers/catalog", "price_areas": "/api/prices/zones",
		"workflow": []string{
			"Read the current draft. Ask only for missing facts and choices.",
			"Discover equipment only when the user asks; reading a draft does not scan.",
			"Patch the current draft revision. Omitted fields and saved credentials remain on the box.",
			"Arrays replace the whole array. Read and retain all existing entries before editing one.",
			"Validate the exact draft, show its changes and assumptions, then ask the user to confirm before commit.",
			"Saving configuration does not prove hardware operation. Check fresh measurements and bounded control tests separately.",
		},
		"normal_use": "Use status, history and control tools for everyday questions. Setup is a separate workflow.",
	})
}

func (s *Server) setupLive() (*config.Config, error) {
	if s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.State == nil {
		return nil, errors.New("setup is not available on this box")
	}
	s.deps.CfgMu.RLock()
	raw, err := json.Marshal(s.deps.Cfg)
	revision := s.deps.Cfg.Revision
	s.deps.CfgMu.RUnlock()
	if err != nil {
		return nil, err
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	cfg.Revision = revision
	cfg.UnresolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	return &cfg, nil
}

func (s *Server) setupError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, onboarding.ErrConflict) {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleSetupDraft(w http.ResponseWriter, _ *http.Request) {
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	d, err := s.setup.Read(live)
	if err != nil {
		s.setupError(w, err)
		return
	}
	s.writeSetupDraft(w, live, d)
}

func (s *Server) writeSetupDraft(w http.ResponseWriter, live *config.Config, d onboarding.Draft) {
	var candidate config.Config
	if err := json.Unmarshal(d.Config, &candidate); err != nil {
		s.setupError(w, err)
		return
	}
	masked := candidate.MaskSecrets()
	maskDriverConfigSecrets(&masked, s.driverSecretKeys())
	validation := ""
	if err := candidate.Validate(); err != nil {
		validation = err.Error()
	}
	receipt, err := s.setup.Receipt()
	if err != nil {
		s.setupError(w, err)
		return
	}
	saved := receipt != nil && receipt.DraftRevision == d.Revision && receipt.SHA256 == d.SHA256
	applied := saved && receipt.SavedRevision == live.Revision
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": d.Revision, "base_revision": d.BaseRevision, "live_revision": live.Revision,
		"sha256": d.SHA256, "config": masked, "stale": !saved && d.BaseRevision != live.Revision,
		"valid": validation == "", "validation_error": validation,
		"saved": saved, "applied": applied, "hardware_verified": false, "last_commit": receipt,
		"restart_reasons": config.RestartRequiredFor(live, &candidate),
	})
}

type setupEdit struct {
	Revision int64           `json:"revision"`
	Patch    json.RawMessage `json:"patch"`
}

func setupJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, onboarding.MaxDraftBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid setup request"})
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send one setup request"})
		return false
	}
	return true
}

func (s *Server) handleSetupEdit(w http.ResponseWriter, r *http.Request) {
	var q setupEdit
	if !setupJSON(w, r, &q) {
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	d, err := s.setup.Edit(live, q.Revision, q.Patch)
	if err != nil {
		s.setupError(w, err)
		return
	}
	s.writeSetupDraft(w, live, d)
}

func (s *Server) handleSetupRestart(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Revision int64 `json:"revision"`
	}
	if !setupJSON(w, r, &q) {
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	d, err := s.setup.Restart(live, q.Revision)
	if err != nil {
		s.setupError(w, err)
		return
	}
	s.writeSetupDraft(w, live, d)
}

type setupCheck struct {
	Revision     int64  `json:"revision"`
	BaseRevision int64  `json:"base_revision"`
	SHA256       string `json:"sha256"`
	Confirmed    bool   `json:"confirmed"`
}

func (s *Server) handleSetupValidate(w http.ResponseWriter, r *http.Request) {
	var q setupCheck
	if !setupJSON(w, r, &q) {
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	if q.BaseRevision != live.Revision {
		s.setupError(w, onboarding.ErrConflict)
		return
	}
	if _, err := s.setup.Checked(live.Revision, q.Revision, q.SHA256); err != nil {
		s.setupError(w, err)
		return
	}
	d, err := s.setup.Read(live)
	if err != nil {
		s.setupError(w, err)
		return
	}
	s.writeSetupDraft(w, live, d)
}

func (s *Server) handleSetupCommit(w http.ResponseWriter, r *http.Request) {
	var q setupCheck
	if !setupJSON(w, r, &q) {
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	previous, err := s.setup.Receipt()
	if err != nil {
		s.setupError(w, err)
		return
	}
	if previous != nil && previous.DraftRevision == q.Revision && previous.SHA256 == q.SHA256 && previous.BaseRevision == q.BaseRevision {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "saved": true,
			"applied": live.Revision == previous.SavedRevision, "live_revision": live.Revision,
			"later_changes": live.Revision > previous.SavedRevision, "commit": previous, "hardware_verified": false})
		return
	}
	if !q.Confirmed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review the checked draft with the user before confirming it"})
		return
	}
	if s.deps.SaveConfigWithValues == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "atomic setup save is not available"})
		return
	}
	if q.BaseRevision != live.Revision {
		s.setupError(w, onboarding.ErrConflict)
		return
	}
	err = s.setup.ApplyChecked(live.Revision, q.Revision, q.SHA256, func(candidate *config.Config) error {
		raw, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		// Reuse Settings' final revision check, durable save and shared applier.
		// This does not dispatch commands, change mode or claim commissioning.
		request := r.Clone(r.Context())
		request.Body = io.NopCloser(bytes.NewReader(raw))
		request.ContentLength = int64(len(raw))
		request.Header = r.Header.Clone()
		request.Header.Set("If-Match", configETag(q.BaseRevision))
		receipt := onboarding.Receipt{DraftRevision: q.Revision, SHA256: q.SHA256,
			BaseRevision: q.BaseRevision, SavedRevision: q.BaseRevision + 1,
			RestartReasons: config.RestartRequiredFor(live, candidate)}
		record, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		s.handlePostConfigWith(w, request, func(path string, cfg *config.Config) error {
			return s.deps.SaveConfigWithValues(path, cfg, map[string]string{onboarding.CommitKey: string(record)})
		})
		return nil
	})
	if err != nil {
		s.setupError(w, err)
	}
}
