package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

const homeWindow = 10 * time.Second
const homeMaxGap = 30 * time.Second
const homeLookback = 3 * time.Minute

// HomeEstimate describes a past interval, never a new instantaneous measurement.
// MinW/MaxW describe variation in the reported window, not error bounds.
// Reported source clocks and local receipt clocks are not verified as aligned.
type HomeEstimate struct {
	Watts       *float64 `json:"watts"`
	MinW        *float64 `json:"window_min_w"`
	MaxW        *float64 `json:"window_max_w"`
	FromMS      int64    `json:"from_ms"`
	ToMS        int64    `json:"to_ms"`
	AgeMS       int64    `json:"age_ms"`
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
	k := key(driver, kind)
	if s.homeObservations == nil {
		s.homeObservations = map[string][]ControlObservation{}
		s.homeSourceTime = map[string]bool{}
	}
	p, ok := ControlPowerObservation(w, data, now)
	var timing struct {
		ControlPowerObservedAt string               `json:"control_power_observed_at"`
		PowerObservedAt        string               `json:"power_observed_at"`
		ForecastPower          *ForecastPowerSample `json:"forecast_power"`
	}
	_ = json.Unmarshal(data, &timing)
	sourceTime := timing.ControlPowerObservedAt != "" || timing.PowerObservedAt != "" || timing.ForecastPower != nil
	if !ok || ((kind == DerEV || kind == DerV2X) && !sourceTime) {
		delete(s.homeObservations, k)
		delete(s.homeSourceTime, k)
		return
	}
	// A successful cloud request can return an old observation. The driver's
	// control confirmation is not a new power timestamp and cannot extend it.
	p.ConfirmedAt = time.Time{}
	points := s.homeObservations[k]
	if !s.health[driver].TelemetryLive() || s.homeSourceTime[k] != sourceTime {
		points = nil
	}
	s.homeSourceTime[k] = sourceTime
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
	cutoff := now.Add(-homeLookback - homeWindow)
	for len(points) > 1 && points[1].At.Before(cutoff) {
		points = points[1:]
	}
	if len(points) > 1024 {
		points = points[len(points)-1024:]
	}
	s.homeObservations[k] = points
}

// HomeUseEstimate integrates a common ten-second window in the reported clocks.
// It uses reported source times, or receipt times for local non-charger flows.
// Clock alignment and device sampling delay remain unverified. Chargers without
// source time, sparse change-only reports and repeats cannot fill a window.
// No extrapolation, guessed fixed delay, clipping, or offline gap repair occurs.
// This path serves display only; ForecastMeasurement and dispatch stay raw.
func (s *Store) HomeUseEstimate(now time.Time, siteMeter string, opts ForecastOptions) HomeEstimate {
	out := HomeEstimate{Method: "reported_time_balance_10s_v2"}
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
	var sourceClocks, receiptClocks bool
	for k, source := range r.Sources {
		points := s.homeObservations[k]
		if (source.Flow.DerType == DerEV || source.Flow.DerType == DerV2X) && !s.homeSourceTime[k] {
			out.Reason = "charger_source_time_unavailable"
			return out
		}
		if source.PowerW == nil || len(points) == 0 || !s.health[source.Flow.Driver].TelemetryLive() {
			out.Reason = "missing_source_window"
			return out
		}
		last := points[len(points)-1]
		if s.homeSourceTime[k] && now.Sub(last.At).Milliseconds() > out.SourceAgeMS {
			out.SourceAgeMS = now.Sub(last.At).Milliseconds()
		}
		if last.At.After(now) || now.Sub(last.At) > homeLookback {
			out.Reason = "stale_source_window"
			return out
		}
		if last.At.Before(to) {
			to = last.At
		}
		sourceClocks = sourceClocks || s.homeSourceTime[k]
		receiptClocks = receiptClocks || !s.homeSourceTime[k]
	}
	out.TimingBasis = "source"
	if receiptClocks {
		out.TimingBasis = "receipt"
		if sourceClocks {
			out.TimingBasis = "mixed"
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
		if i+1 < len(points) {
			next := points[i+1]
			if next.At.After(p.At) && next.At.Sub(p.At) <= homeMaxGap {
				add(p.At, next.At, p.PowerW, next.PowerW)
			}
		}
	}
	return mean / to.Sub(from).Seconds(), low, high, covered == to.Sub(from)
}
