package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

const homeWindow = 10 * time.Second
const homeMaxGap = 30 * time.Second

// HomeEstimate describes a past interval, never a new instantaneous measurement.
// MinW/MaxW bound the timing ambiguity between the observed extrema. They are
// not a confidence interval for sensor accuracy or unknown transport delays.
type HomeEstimate struct {
	Watts  *float64 `json:"watts"`
	MinW   *float64 `json:"timing_min_w"`
	MaxW   *float64 `json:"timing_max_w"`
	FromMS int64    `json:"from_ms"`
	ToMS   int64    `json:"to_ms"`
	AgeMS  int64    `json:"age_ms"`
	Method string   `json:"method"`
	Reason string   `json:"reason,omitempty"`
}

// Keep a separate bounded buffer: neither control evidence nor raw readings may
// depend on how often a client asks for an estimate. Caller holds s.mu.
func (s *Store) recordHomeObservation(driver string, kind DerType, w float64, data json.RawMessage, now time.Time) {
	if kind == DerVehicle {
		return
	}
	k := key(driver, kind)
	if s.homeObservations == nil {
		s.homeObservations = map[string][]ControlObservation{}
	}
	p, ok := ControlPowerObservation(w, data, now)
	if !ok {
		delete(s.homeObservations, k)
		return
	}
	points := s.homeObservations[k]
	if !s.health[driver].TelemetryLive() {
		points = nil
	}
	if len(points) > 0 {
		last := &points[len(points)-1]
		if p.At.Before(last.At) || p.Seen().Before(last.Seen()) || now.Sub(last.Seen()) > homeMaxGap {
			points = nil
		} else if p.At.Equal(last.At) {
			if p.PowerW != last.PowerW {
				delete(s.homeObservations, k)
				return
			}
			// A confirmed change-only sample covers a plateau, not another
			// independent measurement. An unconfirmed repeat adds no time.
			last.ConfirmedAt = p.ConfirmedAt
			s.homeObservations[k] = points
			return
		}
	}
	points = append(points, p)
	if len(points) > 128 {
		points = points[len(points)-128:]
	}
	s.homeObservations[k] = points
}

// HomeUseEstimate integrates every configured flow over the same ten seconds.
// Linear interpolation uses source times when supplied, receipt times otherwise.
// Confirmations extend a constant sample only through the last confirmation.
// No extrapolation, guessed fixed delay, clipping, or offline gap repair occurs.
// This path serves display only; ForecastMeasurement and dispatch stay raw.
func (s *Store) HomeUseEstimate(now time.Time, siteMeter string, opts ForecastOptions) HomeEstimate {
	out := HomeEstimate{Method: "aligned_balance_10s_v1"}
	if s == nil {
		out.Reason = "no_telemetry"
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.forecastMeasurementLocked(now, siteMeter, opts)
	if !r.Valid && r.Reason != "invalid_balance" && r.Reason != "time_skew" {
		out.Reason = r.Reason
		return out
	}
	to := now
	for k, source := range r.Sources {
		points := s.homeObservations[k]
		if source.PowerW == nil || len(points) == 0 || !s.health[source.Flow.Driver].TelemetryLive() {
			out.Reason = "missing_source_window"
			return out
		}
		last := points[len(points)-1]
		if last.Seen().After(now) || now.Sub(last.Seen()) > homeMaxGap {
			out.Reason = "stale_source_window"
			return out
		}
		if last.Seen().Before(to) {
			to = last.Seen()
		}
	}
	from := to.Add(-homeWindow)
	var mean, low, high float64
	for k, source := range r.Sources {
		avg, minW, maxW, ok := homeWindowPower(s.homeObservations[k], from, to)
		if !ok {
			out.Reason = "incomplete_source_window"
			return out
		}
		if source.Flow.DerType == DerMeter {
			mean += avg
			low += minW
			high += maxW
		} else {
			mean -= avg
			low -= maxW
			high -= minW
		}
	}
	out.FromMS, out.ToMS = from.UnixMilli(), to.UnixMilli()
	out.AgeMS = now.Sub(from.Add(homeWindow / 2)).Milliseconds()
	if !finite(mean) || !finite(low) || !finite(high) || mean < 0 {
		out.Reason = "invalid_balance"
		return out
	}
	out.Watts, out.MinW, out.MaxW = &mean, &low, &high
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
		seen := p.Seen()
		if i+1 < len(points) && points[i+1].At.Before(seen) {
			// A late source change can contradict an earlier confirmation.
			// Do not estimate that overlap, but let newer windows recover.
			if points[i+1].At.Before(to) && seen.After(from) {
				return 0, 0, 0, false
			}
			seen = points[i+1].At
		}
		// A confirmation covers the same unchanged value, even when its
		// original change timestamp predates this bounded observation ring.
		if seen.After(p.At) {
			add(p.At, seen, p.PowerW, p.PowerW)
		}
		if i+1 < len(points) {
			next := points[i+1]
			if next.At.Sub(seen) <= homeMaxGap {
				add(seen, next.At, p.PowerW, next.PowerW)
			}
		}
	}
	return mean / to.Sub(from).Seconds(), low, high, covered == to.Sub(from)
}
