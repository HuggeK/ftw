package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/localzone"
	"github.com/srcfl/ftw/go/internal/onboarding"
)

// One identity per process, rather than per API server, so rebuilding the
// HTTP handler cannot claim that restart-only settings became active.
var setupProcessID = uuid.NewString()

func setupRestartPending(receipt *onboarding.Receipt) bool {
	return receipt != nil && len(receipt.RestartReasons) > 0 && (receipt.ProcessID == "" || receipt.ProcessID == setupProcessID)
}

func (s *Server) preserveSetupSecrets(incoming, previous *config.Config) error {
	// Bind each driver's secrets before applying any display-name fallback.
	// Separate pairs also handle two drivers swapping their names in a draft.
	withoutDrivers := *previous
	withoutDrivers.Drivers = nil
	incoming.PreserveMaskedSecrets(&withoutDrivers)
	metadata := s.driverSecretKeys()
	for i, driver := range incoming.Drivers {
		for _, old := range previous.Drivers {
			if driver.Lua != old.Lua || (driver.CredentialOwner != "" && driver.CredentialOwner != old.CredentialOwner) ||
				(driver.CredentialOwner == "" && driver.Name != old.Name) {
				continue
			}
			old.Name = driver.Name
			next, before := config.Config{Drivers: []config.Driver{driver}}, config.Config{Drivers: []config.Driver{old}}
			next.PreserveMaskedSecrets(&before)
			restoreDriverConfigSecrets(&next, &before, metadata)
			incoming.Drivers[i] = next.Drivers[0]
			break
		}
	}
	for _, driver := range incoming.Drivers {
		for key, value := range driver.Config {
			if text, ok := value.(string); ok && text == maskedPlaceholder {
				return fmt.Errorf("driver %q: enter or clear %s after changing the driver; its masked value belongs to the earlier configuration", driver.Name, key)
			}
		}
	}
	return nil
}

func (s *Server) setupRoutes() {
	s.handle("GET /api/setup/schema", Read, s.handleSetupSchema)
	s.handle("GET /api/setup/draft", Local, s.handleSetupDraft)
	s.handle("POST /api/setup/draft", Configure, s.handleSetupEdit)
	s.handle("POST /api/setup/reset", Configure, s.handleSetupReset)
	s.handle("POST /api/setup/validate", Configure, s.handleSetupValidate)
	s.handle("POST /api/setup/commit", Configure, s.handleSetupCommit)
	s.handle("POST /api/setup/restart", Configure, s.handleSetupRestart)
}

func (s *Server) handleSetupSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"driver_catalog": s.driverCatalogReply(), "price_zones": priceZoneItems(), "runtime_timezone": localzone.Name(),
		"schema": onboarding.Schema(), "drivers": "/api/drivers/catalog", "price_areas": "/api/prices/zones",
		"workflow": []string{
			"Read the current draft. Ask only for missing facts and choices.",
			"Discover equipment only when the user asks; reading a draft does not scan.",
			"Patch the current draft revision. Omitted fields and saved credentials remain on the box.",
			"Arrays replace the whole array. Read and retain all existing entries before editing one.",
			"For large lists use JSON Patch add/replace/remove operations against this exact revision; an index never refers to a later draft.",
			"Validate the exact draft, show its changes and assumptions, then ask the user to confirm before commit.",
			"Validation can normalize fields into a new draft revision and checksum. Review that returned configuration and its warnings; an earlier confirmation cannot save the changed draft.",
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
	if err := config.DecodeJSON(raw, &cfg); err != nil {
		return nil, err
	}
	cfg.Revision = revision
	cfg.UnresolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	return &cfg, nil
}

func (s *Server) setupError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	var denied *setupAccessError
	if errors.As(err, &denied) {
		status = denied.status
	}
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
	if err := config.DecodeJSON(d.Config, &candidate); err != nil {
		s.setupError(w, err)
		return
	}
	var validating config.Config
	if err := config.DecodeJSON(d.Config, &validating); err != nil {
		s.setupError(w, err)
		return
	}
	validating.ResolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	validating.UnresolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	validation := ""
	if err := config.PrepareDraftCredentials(&validating, live); err != nil {
		validation = err.Error()
	} else if err := validating.Validate(); err != nil {
		validation = err.Error()
	}
	masked := candidate.MaskSecrets()
	maskDriverConfigSecrets(&masked, s.driverSecretKeys())
	normalizedJSON, err := json.Marshal(&validating)
	if err != nil {
		s.setupError(w, err)
		return
	}
	receipt, err := s.setup.Receipt()
	if err != nil {
		s.setupError(w, err)
		return
	}
	saved := receipt != nil && receipt.DraftRevision == d.Revision && receipt.SHA256 == d.SHA256
	restartPending := setupRestartPending(receipt)
	applied := saved && receipt.SavedRevision == live.Revision && !restartPending
	restartReasons := config.RestartRequiredFor(live, &validating)
	if saved {
		restartReasons = receipt.RestartReasons
	} else if restartPending {
		for _, reason := range receipt.RestartReasons {
			if !slices.Contains(restartReasons, reason) {
				restartReasons = append(restartReasons, reason)
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": d.Revision, "base_revision": d.BaseRevision, "live_revision": live.Revision,
		"normalized_from_revision": d.NormalizedFromRevision, "validation_warnings": d.ValidationWarnings,
		"normalized": d.NormalizedFromRevision > 0,
		"sha256":     d.SHA256, "config": masked, "stale": !saved && d.BaseRevision != live.Revision,
		"valid": validation == "", "validation_error": validation,
		"normalization_required": validation == "" && !bytes.Equal(normalizedJSON, d.Config),
		"saved":                  saved, "applied": applied, "hardware_verified": false, "last_commit": receipt,
		"restart_pending": restartPending, "restart_reasons": restartReasons,
		"next_step": "Review the returned draft after validation, including a new revision, checksum or normalization warnings. Save only that exact reviewed revision. After a completed save, a new draft starts from current settings.",
	})
}

type setupEdit struct {
	Revision   int64           `json:"revision"`
	Patch      json.RawMessage `json:"patch"`
	Operations json.RawMessage `json:"operations"`
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
	if !setupAuthority(w, r) {
		return
	}
	var d onboarding.Draft
	if len(q.Operations) > 0 && len(q.Patch) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send either patch or operations"})
		return
	}
	if len(q.Operations) > 0 {
		d, err = s.setup.EditOperationsWithCheck(live, q.Revision, q.Operations, setupWriteCheck(r))
	} else {
		d, err = s.setup.EditWithCheck(live, q.Revision, q.Patch, setupWriteCheck(r))
	}
	if err != nil {
		s.setupError(w, err)
		return
	}
	s.writeSetupDraft(w, live, d)
}

func (s *Server) handleSetupReset(w http.ResponseWriter, r *http.Request) {
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
	if !setupAuthority(w, r) {
		return
	}
	d, err := s.setup.RestartWithCheck(live, q.Revision, setupWriteCheck(r))
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
	if !setupAuthority(w, r) {
		return
	}
	d, err := s.setup.ValidateWithCheck(live, q.Revision, q.SHA256, setupWriteCheck(r))
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
		s.writeSetupCommit(w, live.Revision, previous)
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
		receipt := onboarding.Receipt{ProcessID: setupProcessID, DraftRevision: q.Revision, SHA256: q.SHA256,
			BaseRevision: q.BaseRevision, SavedRevision: q.BaseRevision + 1,
			RestartReasons: config.RestartRequiredFor(live, candidate)}
		// A later live edit does not rebind listeners or rebuild clients left
		// pending by an earlier setup commit in this process.
		if setupRestartPending(previous) {
			for _, reason := range previous.RestartReasons {
				if !slices.Contains(receipt.RestartReasons, reason) {
					receipt.RestartReasons = append(receipt.RestartReasons, reason)
				}
			}
		}
		record, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		s.handlePostConfigWith(w, request, true, func(path string, cfg *config.Config) error {
			if err := setupWriteCheck(request)(); err != nil {
				return err
			}
			return s.deps.SaveConfigWithValues(path, cfg, map[string]string{onboarding.CommitKey: string(record)}, raw, setupWriteCheck(request))
		}, func(cfg *config.Config, _ []string) {
			s.writeSetupCommit(w, cfg.Revision, &receipt)
		})
		return nil
	})
	if err != nil {
		s.setupError(w, err)
	}
}

func (s *Server) writeSetupCommit(w http.ResponseWriter, liveRevision int64, receipt *onboarding.Receipt) {
	pending := setupRestartPending(receipt)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "saved": true, "applied": liveRevision == receipt.SavedRevision && !pending,
		"live_revision": liveRevision, "restart_required": pending, "restart_reasons": receipt.RestartReasons,
		"later_changes": liveRevision > receipt.SavedRevision, "commit": receipt, "hardware_verified": false,
	})
}
