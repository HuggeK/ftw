// Operator-facing driver controls: send one declared command, hold it for a
// bounded time, and hand the device back to itself when the hold ends.
//
// Deliberately outside control v2. A signed package binds a RuntimePolicy and
// goes through CommandV2 with its write scope, lease and evidence. A bundled
// or local driver has no policy, and giving it a synthesised one would be
// worse than doing nothing: HostEnv.permissionAllowed grants everything only
// while the policy is nil, so a policy without permissions silently blocks
// the driver's own MQTT, and LuaDriver.Command refuses a control v2 driver on
// the legacy path — v2 needs driver_command_v2 entrypoints that no community
// driver has. So this path leaves the policy layer untouched and validates
// against the driver's catalog declaration instead.
//
// What that costs is honest and worth stating: no host-enforced write scope
// and no host-verified evidence. What it keeps is the part that protects
// hardware — Core clamps every value to the declared bounds rather than
// trusting the Lua to do it, and every hold ends by itself.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// A hold has to end on its own. An offset left behind by a browser tab that
// closed, or by an FTW that stopped answering, is a house heated wrong for
// weeks — the failure nobody notices until the bill. 24 h is long enough for
// "warm through the cold snap" and short enough that forgetting is cheap.
const (
	maxControlHoldSeconds     = 24 * 60 * 60
	defaultControlHoldSeconds = 4 * 60 * 60
	controlDefaultTimeout     = 10 * time.Second
)

type controlDriverState struct {
	mu   sync.Mutex
	hold *controlHold
}

// controlHold is one active operator setting. The timer is what releases it;
// the fields are what the UI shows meanwhile.
type controlHold struct {
	Control   string `json:"control"`
	Value     any    `json:"value,omitempty"`
	ExpiresAt int64  `json:"expires_at_ms"`

	timer *time.Timer
}

type controlRequest struct {
	Control   string          `json:"control"`
	Value     json.RawMessage `json:"value"`
	DurationS int             `json:"duration_s"`
}

// POST /api/drivers/{name}/control — send one declared command and hold it.
//
// The command must be declared by the driver's catalog entry. That is the
// whole allowlist: a driver that declares nothing can be commanded by nobody,
// and a typo'd control name is a 400 rather than a silent success. The Lua
// command hook returns no value for an action it does not know, which the
// registry cannot tell apart from a command that worked.
func (s *Server) handleDriverControl(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	var req controlRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}

	declared := s.driverControls(name)
	if len(declared) == 0 {
		writeJSON(w, 404, map[string]string{"error": "driver declares no controls"})
		return
	}
	var control *drivers.CatalogControl
	for i := range declared {
		if declared[i].ID == req.Control {
			control = &declared[i]
			break
		}
	}
	if control == nil {
		writeJSON(w, 400, map[string]string{"error": "unknown control"})
		return
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}

	applied, err := decodeControlValue(req.Value, control.Input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	payload := map[string]any{"action": control.ID, "value": applied}
	if value, ok := applied.(float64); ok {
		// Drivers written before this endpoint read their own key names.
		// Sending both costs one JSON field and saves every such driver a
		// rewrite; heishamon reads cmd.offset or cmd.value.
		payload["offset"] = value
	}

	body, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	seconds := req.DurationS
	if seconds <= 0 {
		seconds = defaultControlHoldSeconds
	}
	if seconds > maxControlHoldSeconds {
		seconds = maxControlHoldSeconds
	}

	// Reserve the hold before dispatch. Registry.Send can return after the
	// request is canceled even while the driver is still applying the command;
	// the reservation guarantees that an ambiguous result still has a bounded
	// safety path. The per-driver lock also keeps expiry/default from racing a
	// replacement command.
	state := s.controlState(name)
	state.mu.Lock()
	defer state.mu.Unlock()
	hold := s.armControlHoldLocked(name, state, control.ID, applied, time.Duration(seconds)*time.Second)
	if err := s.deps.Registry.Send(r.Context(), name, body); err != nil {
		s.clearControlHoldLocked(state)
		defaultCtx, cancel := context.WithTimeout(context.Background(), controlDefaultTimeout)
		defaultErr := s.sendDefaultLocked(defaultCtx, name)
		cancel()
		if defaultErr != nil {
			err = errors.Join(err, fmt.Errorf("restore default after ambiguous command: %w", defaultErr))
			slog.Error("ambiguous driver control could not restore default", "driver", name, "err", defaultErr)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"control":       control.ID,
		"applied":       applied,
		"evidence":      control.Evidence,
		"expires_at_ms": hold.ExpiresAt,
	})
}

// DELETE /api/drivers/{name}/control — end the hold now.
//
// Releasing means calling the driver's own default mode, not writing a value
// this package invented. Only the driver knows what neutral is: heishamon's
// is its configured safe_offset, which an operator may have moved.
func (s *Server) handleDriverControlRelease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}
	if err := s.SendDriverDefault(context.Background(), name); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "released"})
}

func decodeControlValue(raw json.RawMessage, in drivers.CatalogControlInput) (any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("control requires a %s value", in.Type)
	}
	switch in.Type {
	case "number":
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("control requires a numeric value")
		}
		return clampToDeclared(value, in), nil
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("control requires a boolean value")
		}
		return value, nil
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("control requires a string value")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("control has unsupported input type %q", in.Type)
	}
}

func clampToDeclared(value float64, in drivers.CatalogControlInput) float64 {
	if in.Min != nil && value < *in.Min {
		value = *in.Min
	}
	if in.Max != nil && value > *in.Max {
		value = *in.Max
	}
	return value
}

// armControlHold replaces any existing hold for the driver. One driver holds
// one control at a time: two overlapping holds on the same device would each
// expire into a default that undoes the other.
func (s *Server) controlState(name string) *controlDriverState {
	s.controlStateMu.Lock()
	defer s.controlStateMu.Unlock()
	if s.controlStates == nil {
		s.controlStates = make(map[string]*controlDriverState)
	}
	state := s.controlStates[name]
	if state == nil {
		state = &controlDriverState{}
		s.controlStates[name] = state
	}
	return state
}

func (s *Server) armControlHoldLocked(name string, state *controlDriverState, control string, value any, d time.Duration) *controlHold {
	s.clearControlHoldLocked(state)
	hold := &controlHold{
		Control:   control,
		Value:     value,
		ExpiresAt: time.Now().Add(d).UnixMilli(),
	}
	hold.timer = time.AfterFunc(d, func() { s.expireControlHold(name, state, hold) })
	state.hold = hold
	return hold
}

func (s *Server) clearControlHoldLocked(state *controlDriverState) {
	if state.hold != nil && state.hold.timer != nil {
		state.hold.timer.Stop()
	}
	state.hold = nil
}

// expireControlHold hands the device back to itself. It checks identity
// first: a hold that was replaced or released already had its timer stopped,
// but a timer that had begun firing cannot be stopped, and defaulting a
// driver that an operator has just set again is the one wrong answer here.
func (s *Server) expireControlHold(name string, state *controlDriverState, fired *controlHold) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.hold != fired {
		return
	}
	s.clearControlHoldLocked(state)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.sendDefaultLocked(ctx, name); err != nil {
		// Nothing to retry into: the driver is gone, wedged, or already in
		// its default. Log rather than reschedule, so a dead driver does not
		// leave a timer firing every ten seconds for the process lifetime.
		slog.Warn("control hold expiry failed", "driver", name, "err", err)
	}
}

// SendDriverDefault is the shared safety path for watchdogs and API release.
// It clears the operator hold while holding the same per-driver lock used by
// command dispatch, then sends the driver's own default with a bounded
// context. A caller without a deadline gets the 10-second safety deadline.
func (s *Server) SendDriverDefault(ctx context.Context, name string) error {
	state := s.controlState(name)
	state.mu.Lock()
	defer state.mu.Unlock()
	s.clearControlHoldLocked(state)
	return s.sendDefaultLocked(ctx, name)
}

func (s *Server) sendDefaultLocked(ctx context.Context, name string) error {
	if s.deps == nil || s.deps.Registry == nil {
		return errors.New("driver registry not available")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, controlDefaultTimeout)
		defer cancel()
	}
	return s.deps.Registry.SendDefault(ctx, name)
}

func (s *Server) activeControlHold(name string) *controlHold {
	state := s.controlState(name)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.hold == nil {
		return nil
	}
	hold := *state.hold
	hold.timer = nil
	return &hold
}
