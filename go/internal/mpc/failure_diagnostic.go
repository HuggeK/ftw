package mpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
)

const planningFailureByteLimit = 1 << 20

// PlanningFailure is evidence from one attempt, never an executable plan.
// Only the latest failure in this process is retained; a later success marks
// recovery without deleting the inputs needed to investigate the failure.
type PlanningFailure struct {
	SchemaVersion       int             `json:"schema_version"`
	Generation          uint64          `json:"generation"`
	AttemptStartedAtMs  int64           `json:"attempt_started_at_ms"`
	CapturedAtMs        int64           `json:"captured_at_ms"`
	Zone                string          `json:"zone"`
	Trigger             string          `json:"trigger"`
	Stage               string          `json:"stage"`
	Error               string          `json:"error"`
	ErrorTruncated      bool            `json:"error_truncated,omitempty"`
	Inputs              json.RawMessage `json:"inputs"`
	InputsTruncated     bool            `json:"inputs_truncated,omitempty"`
	OptimizerRequest    *FailurePayload `json:"optimizer_request,omitempty"`
	OptimizerResponse   *FailurePayload `json:"optimizer_response,omitempty"`
	RecoveredDecisionID string          `json:"recovered_decision_id,omitempty"`
	RecoveredAtMs       int64           `json:"recovered_at_ms,omitempty"`
}

// A complete JSON payload can be replayed directly. Invalid replies remain
// text; oversized payloads are omitted with their size and a truncation flag.
type FailurePayload struct {
	SizeBytes int             `json:"size_bytes"`
	Truncated bool            `json:"truncated,omitempty"`
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
}

type optimizerFailureError struct {
	cause             error
	request, response *FailurePayload
}

func (e *optimizerFailureError) Error() string { return e.cause.Error() }
func (e *optimizerFailureError) Unwrap() error { return e.cause }

func captureFailurePayload(raw []byte) *FailurePayload {
	if len(raw) == 0 {
		return nil
	}
	p := &FailurePayload{SizeBytes: len(raw), Truncated: len(raw) > planningFailureByteLimit}
	if p.Truncated {
		return p
	} else if json.Valid(raw) {
		p.JSON = append(json.RawMessage(nil), raw...)
	} else {
		p.Text = string(raw)
	}
	return p
}

// PlanningFailureSnapshot returns a copy so support readers cannot alter
// evidence or race a later attempt. It does not replace Diagnose's last
// accepted-plan contract and never grants dispatch permission.
func (s *Service) PlanningFailureSnapshot() *PlanningFailure {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastPlanningFailure == nil {
		return nil
	}
	f := *s.lastPlanningFailure
	f.Inputs = append(json.RawMessage(nil), f.Inputs...)
	clonePayload := func(p *FailurePayload) *FailurePayload {
		if p == nil {
			return nil
		}
		copy := *p
		copy.JSON = append(json.RawMessage(nil), p.JSON...)
		return &copy
	}
	f.OptimizerRequest, f.OptimizerResponse = clonePayload(f.OptimizerRequest), clonePayload(f.OptimizerResponse)
	return &f
}

func (s *Service) recordPlanningFailure(r replanRequest, stage, message string, slots []Slot, p Params, optimizerErr error) {
	if r.wasCanceledByService() {
		return
	}
	// Limit source rows before encoding; normal 48-hour, quarter-hour plans
	// fit in full. The bound never changes the inputs passed to a solver.
	const maxSlots = 512
	count := len(slots)
	if count > maxSlots {
		slots = slots[:maxSlots]
	}
	inputs, truncated := encodeFailureInputs(map[string]any{
		"params": p, "slots": slots, "requested_slots": count,
		"site": r.site, "battery_fleet": r.fleet,
		"non_finite_values": "NaN, +Inf and -Inf are strings",
	})
	f := &PlanningFailure{SchemaVersion: 1, Generation: r.generation,
		AttemptStartedAtMs: r.startedAt.UnixMilli(), CapturedAtMs: time.Now().UnixMilli(),
		Zone: s.Zone, Trigger: r.reason, Stage: stage, Error: message,
		Inputs: inputs, InputsTruncated: truncated || count > maxSlots}
	if len(f.Error) > 8192 {
		// Cutting inside a credential would defeat export redaction. Keep the
		// bounded wire payload instead of an unterminated error fragment.
		f.Error = "Planner error exceeded the 8192-byte diagnostic limit."
		f.ErrorTruncated = true
	}
	var wire *optimizerFailureError
	if errors.As(optimizerErr, &wire) {
		f.OptimizerRequest, f.OptimizerResponse = wire.request, wire.response
	}
	s.mu.Lock()
	if !s.stopping && r.generation == s.latestReplanGeneration && !r.wasCanceledByService() {
		s.lastPlanningFailure = f
	}
	s.mu.Unlock()
}

// Invalid inputs must remain inspectable when normal JSON encoding refuses
// NaN/Inf. Exported fields and JSON exclusions match the ordinary wire shape;
// no value from this diagnostic encoder is used for planning or restore.
func encodeFailureInputs(input any) (json.RawMessage, bool) {
	budget, truncated := planningFailureByteLimit, false
	var value func(reflect.Value) any
	value = func(v reflect.Value) any {
		if !v.IsValid() || ((v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()) {
			return nil
		}
		if budget <= 0 {
			truncated = true
			return nil
		}
		budget -= 32
		if v.CanInterface() {
			if m, ok := v.Interface().(json.Marshaler); ok {
				raw, err := m.MarshalJSON()
				var decoded any
				if err == nil && json.Unmarshal(raw, &decoded) == nil {
					return decoded
				}
			}
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			return value(v.Elem())
		case reflect.Struct:
			out := make(map[string]any)
			for i := 0; i < v.NumField() && budget > 0; i++ {
				field := v.Type().Field(i)
				name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
				if !field.IsExported() || name == "-" || (strings.Contains(options, "omitempty") && v.Field(i).IsZero()) {
					continue
				}
				if name == "" {
					name = field.Name
				}
				out[name] = value(v.Field(i))
			}
			return out
		case reflect.Slice, reflect.Array:
			out := make([]any, 0, min(v.Len(), max(0, budget/32)))
			for i := 0; i < v.Len() && budget > 0; i++ {
				out = append(out, value(v.Index(i)))
			}
			return out
		case reflect.Map:
			out := make(map[string]any)
			iter := v.MapRange()
			for iter.Next() && budget > 0 {
				out[fmt.Sprint(iter.Key().Interface())] = value(iter.Value())
			}
			return out
		case reflect.Float32, reflect.Float64:
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Sprint(f)
			}
			return f
		case reflect.String:
			text := v.String()
			if len(text) > max(0, budget) {
				text, truncated = text[:max(0, budget)], true
			}
			budget -= len(text)
			return text
		default:
			return v.Interface()
		}
	}
	raw, err := json.Marshal(value(reflect.ValueOf(input)))
	if err != nil || len(raw) > planningFailureByteLimit {
		return json.RawMessage(`{"capture_error":"planning inputs exceed the diagnostic encoding limit"}`), true
	}
	return raw, truncated || budget <= 0
}
