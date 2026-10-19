package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/onboarding"
	"github.com/srcfl/ftw/go/internal/scanner"
)

func (s *Server) handleSetupPrepare(w http.ResponseWriter, r *http.Request, q setupEdit) {
	if !q.Confirmed || len(q.Patch) > 0 || len(q.Operations) > 0 {
		writeJSON(w, 400, map[string]string{"error": "confirm one setup action without a config patch"})
		return
	}
	if q.Action != "discover" && q.Action != "test" && q.Action != "connect" {
		writeJSON(w, 400, map[string]string{"error": "unknown setup action"})
		return
	}
	live, err := s.setupLive()
	if err != nil {
		s.setupError(w, err)
		return
	}
	check := func() error {
		if err := setupWriteCheck(r)(); err != nil {
			return err
		}
		current, err := s.setupLive()
		if err != nil {
			return err
		}
		if current.Revision != live.Revision {
			return onboarding.ErrConflict
		}
		return nil
	}
	var result any
	var secrets []string
	if q.Action == "discover" {
		if q.Driver != "" || q.RedirectURI != "" {
			s.setupError(w, errors.New("discovery does not take a driver or redirect URI"))
			return
		}
		before, err := s.setup.Read(live)
		if err != nil {
			s.setupError(w, err)
			return
		}
		if before.Revision != q.Revision || before.BaseRevision != live.Revision {
			s.setupError(w, onboarding.ErrConflict)
			return
		}
		if err := check(); err != nil {
			s.setupError(w, err)
			return
		}
		devices, err := scanner.Scan(r.Context())
		if err != nil {
			s.setupError(w, err)
			return
		}
		if q.Fingerprint {
			result = s.enrichScanWithFingerprintsContext(r.Context(), devices)
		} else {
			result = devices
		}
		if err := check(); err != nil {
			s.setupError(w, err)
			return
		}
		after, err := s.setup.Read(live)
		if err != nil {
			s.setupError(w, err)
			return
		}
		if before.Revision != after.Revision || before.SHA256 != after.SHA256 {
			s.setupError(w, onboarding.ErrConflict)
			return
		}
		writeJSON(w, 200, map[string]any{"action": q.Action, "revision": after.Revision, "result": result, "hardware_control_verified": false,
			"next": "Review found devices before adding them to the draft. Discovery does not select, configure or command equipment."})
		return
	}
	d, err := s.setup.Prepare(live, q.Revision, func(candidate *config.Config) error {
		if q.Fingerprint {
			return errors.New("fingerprint is only for discovery")
		}
		var selected *config.Driver
		for i := range candidate.Drivers {
			if candidate.Drivers[i].Name == q.Driver {
				selected = &candidate.Drivers[i]
				break
			}
		}
		if selected == nil {
			return errors.New("select a driver in this draft first")
		}
		secrets = s.setupSecretValues(candidate)
		if q.Action == "connect" {
			result, err = s.startSetupMyUplink(r, *selected, q.Revision, live.Revision, q.RedirectURI)
			return err
		}
		if q.RedirectURI != "" {
			return errors.New("a connection test does not take a redirect URI")
		}
		for _, saved := range live.Drivers {
			if saved.SecretOwner() != selected.SecretOwner() {
				continue
			}
			token, _ := saved.Config["refresh_token"].(string)
			_, rotated := s.deps.State.LoadConfig(driverSecretStateKey(saved.SecretOwner(), "refresh_token"))
			if token == "" && !rotated {
				continue
			}
			// A second OAuth reader can rotate the running driver's token
			// family. Inspect the existing reader instead of forking it.
			answer := map[string]any{"state": "shared_live_credentials", "existing_driver": saved.Name, "probe_was_run": false, "draft_connection_verified": false,
				"note": "This draft retains the running driver's token family. Inspect its current readings and health. A separate probe could revoke its token. Changes in the draft have not been tested; use vendor sign-in to connect a different account."}
			if s.deps.Tel != nil {
				answer["health"] = s.deps.Tel.DriverHealth(saved.Name)
				answer["metrics"] = s.deps.Tel.LatestMetricsByDriver(saved.Name)
			}
			result = answer
			return nil
		}
		capture := &setupCapture{header: http.Header{}}
		s.runDriverTest(capture, r, *selected, func(reg *drivers.Registry, testName string) {
			reg.ControlBlocked = func() bool { return true }
			// Token rotation belongs to this exact draft. Never save it to
			// live Settings or reuse another driver's credential namespace.
			var mu sync.Mutex
			reg.SecretPersister = func(name, key, value string) error {
				mu.Lock()
				defer mu.Unlock()
				if name != testName {
					return errors.New("probe credential owner changed")
				}
				if selected.Config == nil {
					selected.Config = map[string]any{}
				}
				selected.Config[key] = value
				return nil
			}
		})
		if capture.overflow {
			return errors.New("connection test answer is too large")
		}
		var answer any
		dec := json.NewDecoder(bytes.NewReader(capture.body.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&answer); err != nil {
			return errors.New("invalid connection test answer")
		}
		redactSensitive(answer)
		result = map[string]any{"status": capture.status, "response": answer}
		secrets = append(secrets, s.setupSecretValues(candidate)...)
		return nil
	}, check)
	if err != nil {
		s.setupError(w, setupScrubbedError{err, scrubSetupResult(err.Error(), secrets).(string)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": q.Action, "revision": d.Revision, "base_revision": d.BaseRevision, "sha256": d.SHA256,
		"result": scrubSetupResult(result, secrets), "hardware_control_verified": false,
		"next": "Read setup_status to continue from this draft. A discovery or connection test does not select equipment, apply settings or prove control. Vendor sign-in saves credentials to this draft only.",
	})
}

// setupCapture reuses the existing bounded connection probe without making
// an HTTP request or replacing the authenticated caller.
type setupCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (c *setupCapture) Header() http.Header    { return c.header }
func (c *setupCapture) WriteHeader(status int) { c.status = status }
func (c *setupCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(p) > onboarding.MaxDraftBytes {
		c.overflow = true
		return 0, fmt.Errorf("setup result too large")
	}
	return c.body.Write(p)
}

func (s *Server) setupSecretValues(cfg *config.Config) []string {
	masked := cfg.MaskSecrets()
	maskDriverConfigSecrets(&masked, s.driverSecretKeys())
	before, _ := json.Marshal(cfg)
	after, _ := json.Marshal(masked)
	var plain, hidden any
	_ = json.Unmarshal(before, &plain)
	_ = json.Unmarshal(after, &hidden)
	values := []string{}
	var walk func(any, any)
	walk = func(a, b any) {
		switch v := a.(type) {
		case map[string]any:
			other, _ := b.(map[string]any)
			for key, val := range v {
				walk(val, other[key])
			}
		case []any:
			other, _ := b.([]any)
			for i, val := range v {
				if i < len(other) {
					walk(val, other[i])
				}
			}
		case string:
			masked, hidden := b.(string)
			if (b == nil || hidden && (masked == maskedPlaceholder || masked == "")) && v != "" && v != maskedPlaceholder {
				values = append(values, v)
			}
		}
	}
	walk(plain, hidden)
	return values
}

type setupScrubbedError struct {
	cause   error
	message string
}

func (e setupScrubbedError) Error() string { return e.message }
func (e setupScrubbedError) Unwrap() error { return e.cause }

func scrubSetupResult(result any, secrets []string) any {
	switch v := result.(type) {
	case string:
		for _, secret := range secrets {
			v = strings.ReplaceAll(v, secret, maskedPlaceholder)
		}
		return v
	case map[string]any:
		for key, val := range v {
			v[key] = scrubSetupResult(val, secrets)
		}
		return v
	case []any:
		for i, val := range v {
			v[i] = scrubSetupResult(val, secrets)
		}
		return v
	default:
		return result
	}
}
