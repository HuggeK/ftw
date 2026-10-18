package telemetry

import (
	"encoding/json"
	"time"
)

type homeEnergyPoint struct {
	At time.Time
	Wh float64
}

type homeEnergySeries struct {
	Session string
	Points  []homeEnergyPoint
}

// Easee already supplies these three fields together. A counter without its
// observation time cannot distinguish a fresh read from a cached slow register.
// In particular, do not receipt-stamp local counters or mix battery DC energy
// with the AC power used in the site balance. Those flows use sampled power.
func (s *Store) recordHomeEnergy(driver string, kind DerType, data json.RawMessage, now time.Time) {
	if kind != DerEV {
		return
	}
	k := key(driver, kind)
	var d struct {
		Wh      *float64 `json:"session_wh"`
		At      string   `json:"energy_observed_at"`
		Session string   `json:"session_id"`
	}
	err := json.Unmarshal(data, &d)
	series := s.homeEnergy[k]
	if series == nil {
		if d.At == "" {
			return
		}
		if s.homeEnergy == nil {
			s.homeEnergy = map[string]*homeEnergySeries{}
		}
		series = &homeEnergySeries{}
		s.homeEnergy[k] = series
	}
	at, timeErr := time.Parse(time.RFC3339Nano, d.At)
	if err != nil || timeErr != nil || at.After(now) || d.Wh == nil || !finite(*d.Wh) || *d.Wh < 0 || d.Session == "" {
		series.Points = nil
		return
	}
	if !s.health[driver].TelemetryLive() || series.Session != d.Session {
		series.Points = nil
	}
	series.Session = d.Session
	p := homeEnergyPoint{At: at, Wh: *d.Wh}
	points := series.Points
	if len(points) > 0 {
		last := points[len(points)-1]
		if at.Before(last.At) || (at.Equal(last.At) && p.Wh != last.Wh) {
			series.Points = nil
			return
		}
		if at.Equal(last.At) {
			return // A poll of the same observation adds neither energy nor time.
		}
		if p.Wh < last.Wh || at.Sub(last.At) > homeMaxWindow {
			points = nil // Reset, missed session boundary, or unsupported long gap.
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
	series.Points = points
}

// Counter differences give energy over their own interval. At an interior
// boundary, linear cumulative-energy interpolation is an estimate, not evidence
// of constant instantaneous power. Never extend beyond the reported endpoints.
func homeCounterEnergy(points []homeEnergyPoint, from, to time.Time) (float64, bool) {
	energyAt := func(at time.Time) (float64, bool) {
		for i, p := range points {
			if p.At.Equal(at) {
				return p.Wh, true
			}
			if i+1 < len(points) {
				next := points[i+1]
				if p.At.Before(at) && next.At.After(at) && next.At.Sub(p.At) <= homeMaxWindow {
					f := at.Sub(p.At).Seconds() / next.At.Sub(p.At).Seconds()
					return p.Wh + (next.Wh-p.Wh)*f, true
				}
			}
		}
		return 0, false
	}
	a, okA := energyAt(from)
	b, okB := energyAt(to)
	return b - a, okA && okB && to.After(from) && finite(b-a) && b >= a
}
