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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
)

// controlHold is one active operator setting. The timer is what releases it;
// the fields are what the UI shows meanwhile.
type controlHold struct {
	Control   string   `json:"control"`
	Value     *float64 `json:"value,omitempty"`
	ExpiresAt int64    `json:"expires_at_ms"`

	timer *time.Timer
}

type controlRequest struct {
	Control   string   `json:"control"`
	Value     *float64 `json:"value"`
	DurationS int      `json:"duration_s"`
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

	payload := map[string]any{"action": control.ID}
	var applied *float64
	if control.Input.Type == "number" {
		if req.Value == nil {
			writeJSON(w, 400, map[string]string{"error": "control requires a value"})
			return
		}
		// Clamp here rather than reject: the bounds came from the driver, and
		// a UI that rounds differently should not fail an operator's click.
		// Clamping in Core is the point — a driver that forgets to clamp is
		// exactly the driver this protects.
		value := clampToDeclared(*req.Value, control.Input)
		applied = &value
		payload["value"] = value
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
	if err := s.deps.Registry.Send(r.Context(), name, body); err != nil {
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
	hold := s.armControlHold(name, control.ID, applied, time.Duration(seconds)*time.Second)

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
	s.clearControlHold(name)
	if err := s.deps.Registry.SendDefault(r.Context(), name); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "released"})
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
func (s *Server) armControlHold(name, control string, value *float64, d time.Duration) *controlHold {
	s.controlHoldMu.Lock()
	defer s.controlHoldMu.Unlock()
	if s.controlHolds == nil {
		s.controlHolds = make(map[string]*controlHold)
	}
	if existing, ok := s.controlHolds[name]; ok && existing.timer != nil {
		existing.timer.Stop()
	}
	hold := &controlHold{
		Control:   control,
		Value:     value,
		ExpiresAt: time.Now().Add(d).UnixMilli(),
	}
	hold.timer = time.AfterFunc(d, func() { s.expireControlHold(name, hold) })
	s.controlHolds[name] = hold
	return hold
}

// expireControlHold hands the device back to itself. It checks identity
// first: a hold that was replaced or released already had its timer stopped,
// but a timer that had begun firing cannot be stopped, and defaulting a
// driver that an operator has just set again is the one wrong answer here.
func (s *Server) expireControlHold(name string, fired *controlHold) {
	s.controlHoldMu.Lock()
	current, ok := s.controlHolds[name]
	if !ok || current != fired {
		s.controlHoldMu.Unlock()
		return
	}
	delete(s.controlHolds, name)
	s.controlHoldMu.Unlock()

	if s.deps.Registry == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.deps.Registry.SendDefault(ctx, name); err != nil {
		// Nothing to retry into: the driver is gone, wedged, or already in
		// its default. Log rather than reschedule, so a dead driver does not
		// leave a timer firing every ten seconds for the process lifetime.
		slog.Warn("control hold expiry failed", "driver", name, "err", err)
	}
}

func (s *Server) clearControlHold(name string) {
	s.controlHoldMu.Lock()
	defer s.controlHoldMu.Unlock()
	if hold, ok := s.controlHolds[name]; ok {
		if hold.timer != nil {
			hold.timer.Stop()
		}
		delete(s.controlHolds, name)
	}
}

func (s *Server) activeControlHold(name string) *controlHold {
	s.controlHoldMu.Lock()
	defer s.controlHoldMu.Unlock()
	return s.controlHolds[name]
}
