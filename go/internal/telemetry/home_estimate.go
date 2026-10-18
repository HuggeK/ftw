package telemetry

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

const homeWindow = 10 * time.Second
const homeMaxGap = 30 * time.Second
const homeLookback = 3 * time.Minute
const homeMaxWindow = 5 * time.Minute

// HomeEstimate describes a past interval, never a new instantaneous measurement.
// MinW/MaxW describe variation in the reported window, not error bounds.
// Reported source clocks and local receipt clocks are not verified as aligned.
type HomeEstimate struct {
	Watts       *float64 `json:"watts"`
	EnergyWh    *float64 `json:"energy_wh"`
	EnergyBasis string   `json:"energy_basis"`
	MinW        *float64 `json:"window_min_w"`
	MaxW        *float64 `json:"window_max_w"`
	FromMS      int64    `json:"from_ms"`
	ToMS        int64    `json:"to_ms"`
	AgeMS       int64    `json:"age_ms"`
	EndAgeMS    int64    `json:"end_age_ms"`
	Method      string   `json:"method"`
	TimingBasis string   `json:"timing_basis"`
	SourceAgeMS int64    `json:"source_age_ms"`
	Reason      string   `json:"reason,omitempty"`
}

// Keep a separate bounded buffer: neither control evidence nor raw readings may
// depend on how often a client asks for an estimate. Caller holds s.mu.
func (s *Store) recordHomeObservation(driver string, kind DerType, w float64, data json.RawMessage, now time.Time) {
	if kind == DerVehicle {
		return
	}
	s.recordHomeEnergy(driver, kind, data, now)
	k := key(driver, kind)
	if s.homeObservations == nil {
		s.homeObservations = map[string][]ControlObservation{}
		s.homeSourceTime = map[string]bool{}
		s.homeIdleState = map[string]time.Time{}
	}
	p, ok := ControlPowerObservation(w, data, now)
	var timing struct {
		ControlPowerObservedAt string               `json:"control_power_observed_at"`
		PowerObservedAt        string               `json:"power_observed_at"`
		ForecastPower          *ForecastPowerSample `json:"forecast_power"`
	}
	_ = json.Unmarshal(data, &timing)
	sourceTime := timing.ControlPowerObservedAt != "" || timing.PowerObservedAt != "" || timing.ForecastPower != nil
	idleState := time.Time{}
	if ok && kind == DerEV && w == 0 && p.PowerW == 0 {
		idleState = homeDisconnectedState(data, now)
	}
	if !ok || ((kind == DerEV || kind == DerV2X) && !sourceTime && idleState.IsZero()) {
		delete(s.homeObservations, k)
		delete(s.homeSourceTime, k)
		delete(s.homeIdleState, k)
		return
	}
	if !idleState.IsZero() {
		// A confirmed disconnected poll supports a zero-EV-load estimate at
		// receipt time. It does not refresh the old power/session observation.
		// Bracket only these polls, and start over after any state transition.
		p.At, sourceTime = now, false
		delete(s.homeEnergy, k)
	}
	// A successful cloud request can return an old observation. The driver's
	// control confirmation is not a new power timestamp and cannot extend it.
	p.ConfirmedAt = time.Time{}
	points := s.homeObservations[k]
	if !s.health[driver].TelemetryLive() || s.homeSourceTime[k] != sourceTime || !s.homeIdleState[k].Equal(idleState) {
		points = nil
	}
	s.homeSourceTime[k] = sourceTime
	s.homeIdleState[k] = idleState
	if len(points) > 0 {
		last := &points[len(points)-1]
		if p.At.Before(last.At) || p.At.Sub(last.At) > homeMaxGap {
			points = nil
		} else if p.At.Equal(last.At) {
			if p.PowerW != last.PowerW {
				delete(s.homeObservations, k)
				return
			}
			// A repeated change-only timestamp adds no time, even when a
			// fresh HTTP reply carries a control confirmation.
			s.homeObservations[k] = points
			return
		}
	}
	points = append(points, p)
	cutoff := now.Add(-homeLookback - homeMaxWindow)
	for len(points) > 1 && points[1].At.Before(cutoff) {
		points = points[1:]
	}
	if len(points) > 1024 {
		points = points[len(points)-1024:]
	}
	s.homeObservations[k] = points
}

func homeDisconnectedState(data json.RawMessage, now time.Time) time.Time {
	var d struct {
		Connected             *bool  `json:"connected"`
		Charging              *bool  `json:"charging"`
		ControlPowerAvailable bool   `json:"control_power_available"`
		ControlPowerConfirmed bool   `json:"control_power_confirmed"`
		StateObservedAt       string `json:"state_observed_at"`
		SessionID             string `json:"session_id"`
	}
	if json.Unmarshal(data, &d) != nil || d.Connected == nil || *d.Connected || d.Charging == nil || *d.Charging || !d.ControlPowerAvailable || !d.ControlPowerConfirmed || d.SessionID != "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, d.StateObservedAt)
	if err != nil || at.After(now) {
		return time.Time{}
	}
	return at
}

// HomeUseEstimate divides signed energy by a common reported interval. Prefer
// timestamped session counters; otherwise integrate bracketed power. The period
// is at least ten seconds and at least the largest of each source's last three
// distinct reporting intervals. This is observed coverage, not a cadence promise.
// Clocks and sampling delay remain unverified. This path serves display only.
func (s *Store) HomeUseEstimate(now time.Time, siteMeter string, opts ForecastOptions) HomeEstimate {
	out := HomeEstimate{Method: "reported_energy_balance_v3"}
	if s == nil {
		out.Reason = "no_telemetry"
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.forecastMeasurementLocked(now, siteMeter, opts)
	if !r.Valid && r.Reason != "invalid_balance" && r.Reason != "time_skew" && !strings.HasPrefix(r.Reason, "stale:") && !strings.HasPrefix(r.Reason, "unknown_power:") {
		out.Reason = r.Reason
		return out
	}
	to := now
	window := homeWindow
	counterCount := 0
	var reference *homeEnergySeries
	var referenceKey string
	var referenceGap time.Duration
	var sourceClocks, receiptClocks bool
	for k, source := range r.Sources {
		points := s.homeObservations[k]
		counter := s.homeEnergy[k]
		if counter == nil && (source.Flow.DerType == DerEV || source.Flow.DerType == DerV2X) && !s.homeSourceTime[k] && s.homeIdleState[k].IsZero() {
			out.Reason = "charger_source_time_unavailable"
			return out
		}
		if !s.health[source.Flow.Driver].TelemetryLive() {
			out.Reason = "missing_source_window"
			return out
		}
		var times []time.Time
		usesSourceTime := s.homeSourceTime[k]
		if counter != nil {
			for _, p := range counter.Points {
				times = append(times, p.At)
			}
			usesSourceTime = true
			counterCount++
		} else {
			for _, p := range points {
				times = append(times, p.At)
			}
		}
		if len(times) < 2 {
			out.Reason = "incomplete_source_window"
			return out
		}
		last := times[len(times)-1]
		if usesSourceTime && now.Sub(last).Milliseconds() > out.SourceAgeMS {
			out.SourceAgeMS = now.Sub(last).Milliseconds()
		}
		if last.After(now) || now.Sub(last) > homeLookback {
			out.Reason = "stale_source_window"
			return out
		}
		if last.Before(to) {
			to = last
		}
		var reportGap time.Duration
		for i := len(times) - 1; i > 0 && i >= len(times)-3; i-- {
			if gap := times[i].Sub(times[i-1]); gap > reportGap {
				reportGap = gap
			}
		}
		if reportGap > window {
			window = reportGap
		}
		if counter != nil && (reference == nil || reportGap > referenceGap || (reportGap == referenceGap && k < referenceKey)) {
			reference, referenceKey, referenceGap = counter, k, reportGap
		}
		sourceClocks = sourceClocks || usesSourceTime
		receiptClocks = receiptClocks || !usesSourceTime
	}
	out.TimingBasis = "source"
	if receiptClocks {
		out.TimingBasis = "receipt"
		if sourceClocks {
			out.TimingBasis = "mixed"
		}
	}
	from := to.Add(-window)
	if reference != nil {
		// Keep the slowest counter's complete interval wherever possible.
		// Other counter clocks may require cumulative-energy interpolation.
		endFound, startFound := false, false
		for i := len(reference.Points) - 1; i >= 0; i-- {
			at := reference.Points[i].At
			if !endFound && !at.After(to) {
				to, endFound = at, true
				from = to.Add(-window)
			}
			if endFound && !at.After(from) {
				from, startFound = at, true
				break
			}
		}
		if !startFound || to.Sub(from) > homeMaxWindow || now.Sub(to) > homeLookback {
			out.Reason = "incomplete_source_window"
			return out
		}
		window = to.Sub(from)
	}
	out.FromMS, out.ToMS = from.UnixMilli(), to.UnixMilli()
	out.AgeMS = now.Sub(from.Add(window / 2)).Milliseconds()
	out.EndAgeMS = now.Sub(to).Milliseconds()
	out.EnergyBasis = "power_integral"
	if counterCount > 0 {
		out.EnergyBasis = "counters"
		if counterCount < len(r.Sources) {
			out.EnergyBasis = "mixed"
		}
	}
	var energy, low, high float64
	for k, source := range r.Sources {
		var wh, minW, maxW float64
		var ok bool
		if counter := s.homeEnergy[k]; counter != nil {
			wh, ok = homeCounterEnergy(counter.Points, from, to)
		} else {
			var avg float64
			avg, minW, maxW, ok = homeWindowPower(s.homeObservations[k], from, to)
			wh = avg * window.Hours()
		}
		if !ok {
			out.Reason = "incomplete_source_window"
			return out
		}
		if source.Flow.DerType == DerMeter {
			energy += wh
			low += minW
			high += maxW
		} else {
			energy -= wh
			low -= maxW
			high -= minW
		}
	}
	mean := energy / window.Hours()
	if !finite(mean) || !finite(low) || !finite(high) || mean < 0 {
		out.Reason = "invalid_balance"
		return out
	}
	out.Watts, out.EnergyWh = &mean, &energy
	if counterCount == 0 {
		out.MinW, out.MaxW = &low, &high
	}
	return out
}

func homeWindowPower(points []ControlObservation, from, to time.Time) (mean, low, high float64, ok bool) {
	covered := time.Duration(0)
	low, high = math.Inf(1), math.Inf(-1)
	add := func(a, b time.Time, wa, wb float64) {
		left, right := a, b
		if left.Before(from) {
			left = from
		}
		if right.After(to) {
			right = to
		}
		if !right.After(left) {
			return
		}
		span := b.Sub(a).Seconds()
		v0 := wa + (wb-wa)*left.Sub(a).Seconds()/span
		v1 := wa + (wb-wa)*right.Sub(a).Seconds()/span
		mean += (v0 + v1) / 2 * right.Sub(left).Seconds()
		low = math.Min(low, math.Min(v0, v1))
		high = math.Max(high, math.Max(v0, v1))
		covered += right.Sub(left)
	}
	for i, p := range points {
		if i+1 < len(points) {
			next := points[i+1]
			if next.At.After(p.At) && next.At.Sub(p.At) <= homeMaxGap {
				add(p.At, next.At, p.PowerW, next.PowerW)
			}
		}
	}
	return mean / to.Sub(from).Seconds(), low, high, covered == to.Sub(from)
}
