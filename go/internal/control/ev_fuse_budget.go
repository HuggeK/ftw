package control

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evHeadroomW is the shared grid balance for an EV pool. Power outside the
// pool stays in gridW. No unmeasured future battery relief earns headroom.
func evHeadroomW(ceiling, gridW, poolW float64) float64 {
	if !finiteFuseValue(ceiling) || !finiteFuseValue(gridW) || !finiteFuseValue(poolW) || ceiling <= 0 || poolW < 0 {
		return 0
	}
	return math.Max(0, ceiling-gridW+poolW)
}

func finiteFuseValue(w float64) bool { return !math.IsNaN(w) && !math.IsInf(w, 0) }

// EVFuseBudget refreshes the common EV cap before a charger command, also
// without batteries. The shared freshness gate must already allow dispatch.
// Other EVs remain measured household demand. Callers hold the control lock.
func EVFuseBudget(store *telemetry.Store, state *State, points []loadpoint.State, configs []loadpoint.Config, fuseMaxW float64) (float64, bool) {
	if store == nil || state == nil || !finiteFuseValue(fuseMaxW) || fuseMaxW <= 0 {
		return 0, true
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil || !finiteFuseValue(meter.RawW) {
		return 0, true
	}
	gridW, poolW := coherentEVBalance(store, state.SiteMeterDriver, meter, points, state.now())
	cap := evHeadroomW(state.phaseImportCeilingW(fuseMaxW, meter), gridW, poolW)
	if phaseW, known := evPhaseIncreaseW(meter, state, configs, poolW); known {
		cap = math.Min(cap, math.Max(0, poolW+phaseW))
	}
	if state.FuseSaturated {
		cap = math.Min(cap, state.FuseEVMaxW)
	}
	return cap, true
}

// A managed EV earns replacement credit only across a site observation that
// its raw power brackets. This keeps normal asynchronous polls useful while
// preventing a later start or an earlier stop from inventing spare capacity.
// The existing bounded telemetry ring owns the samples; no second cache or
// filtered load estimate participates in the hard limit.
func coherentEVBalance(store *telemetry.Store, meterDriver string, meter *telemetry.DerReading, points []loadpoint.State, now time.Time) (gridW, poolW float64) {
	gridW = meter.RawW
	from := now.Add(-telemetry.ControlWindowDuration)
	windows := store.ControlWindows(from, now)
	sites := windows[meterDriver+":"+telemetry.DerMeter.String()].Points
	if current, ok := qualifiedFuseObservation(meter, now); ok {
		current.At = current.Seen()
		sites = append(sites, current)
	}
	type evSeries struct {
		latest float64
		points []telemetry.ControlObservation
	}
	var series []evSeries
	seen := map[string]bool{}
	for _, point := range points {
		if !point.PluggedIn || seen[point.DriverName] {
			continue
		}
		seen[point.DriverName] = true
		r := store.Get(point.DriverName, telemetry.DerEV)
		h := store.DriverHealth(point.DriverName)
		current, ok := qualifiedFuseObservation(r, now)
		if !ok || h == nil || !h.TelemetryLive() || current.PowerW <= 0 {
			continue // Unknown EV power stays inside the measured site demand.
		}
		ps := windows[point.DriverName+":"+telemetry.DerEV.String()].Points
		ps = append(ps, current) // Includes explicit change-only confirmations.
		series = append(series, evSeries{math.Min(current.PowerW, r.RawW), ps})
	}
	sort.SliceStable(sites, func(i, j int) bool { return sites[i].At.After(sites[j].At) })
	for _, site := range sites {
		if site.At.Before(from) || site.At.After(now) || !finiteFuseValue(site.PowerW) {
			continue
		}
		credit, coherent := 0.0, true
		for _, ev := range series {
			w, ok := bracketedEVPower(ev.points, site.At, ev.latest)
			if !ok {
				coherent = false
				break
			}
			credit += w
		}
		if coherent {
			// A fresh increase in house/site demand always wins over an
			// older coherent sample. A fresh EV drop already reduced credit.
			return math.Max(gridW, site.PowerW), credit
		}
	}
	return gridW, 0
}

func qualifiedFuseObservation(r *telemetry.DerReading, now time.Time) (telemetry.ControlObservation, bool) {
	if r == nil || !finiteFuseValue(r.RawW) || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > telemetry.ControlPowerMaxAge(r.Data) {
		return telemetry.ControlObservation{}, false
	}
	p, ok := telemetry.ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	return p, ok && !p.Seen().After(now) && now.Sub(p.Seen()) <= telemetry.ControlWindowDuration
}

func bracketedEVPower(points []telemetry.ControlObservation, at time.Time, latest float64) (float64, bool) {
	var before, after telemetry.ControlObservation
	hasBefore, hasAfter := false, false
	for _, p := range points {
		if !finiteFuseValue(p.PowerW) || p.PowerW < 0 {
			continue
		}
		if !p.At.After(at) && (!hasBefore || p.At.After(before.At)) {
			before, hasBefore = p, true
		}
		if !p.Seen().Before(at) && (!hasAfter || p.At.Before(after.At)) {
			after, hasAfter = p, true
		}
	}
	if !hasBefore || !hasAfter {
		return 0, false
	}
	return math.Max(0, math.Min(latest, math.Min(before.PowerW, after.PowerW))), true
}

// Phase-current magnitudes cannot tell us that an exporting phase makes
// room on an importing one. Bound the actual increase on the worst phase.
func evPhaseIncreaseW(meter *telemetry.DerReading, state *State, configs []loadpoint.Config, poolW float64) (float64, bool) {
	// DER data uses snake case; separate fields retain absent-vs-zero.
	var data struct {
		L1 *float64 `json:"l1_a"`
		L2 *float64 `json:"l2_a"`
		L3 *float64 `json:"l3_a"`
	}
	if json.Unmarshal(meter.Data, &data) != nil {
		return 0, false
	}
	amps := [...]*float64{data.L1, data.L2, data.L3}
	known := data.L1 != nil || data.L2 != nil || data.L3 != nil
	if !known {
		return 0, false
	}
	if !finiteFuseValue(state.SiteFuseAmps) || !finiteFuseValue(state.SiteFuseVoltage) || state.SiteFuseAmps <= 0 || state.SiteFuseVoltage <= 0 || state.SiteFusePhases < 1 || state.SiteFusePhases > 3 {
		return 0, true
	}
	worst := 0.0
	for _, a := range amps[:state.SiteFusePhases] {
		if a == nil || !finiteFuseValue(*a) {
			return 0, true
		}
		worst = math.Max(worst, math.Abs(*a))
	}
	phases := state.SiteFusePhases
	unknownPhase := false
	for _, cfg := range configs {
		if cfg.PhaseMode == "1p" || cfg.PhaseMode == "auto" {
			phases = 1 // No promise about an auto charger's current phase dwell.
			unknownPhase = true
		}
	}
	deltaA := state.SiteFuseAmps - 1 - worst
	if deltaA < 0 && unknownPhase {
		return -poolW, true // Cannot prove which single-phase car relieves it.
	}
	return deltaA * state.SiteFuseVoltage * float64(phases), true
}

func publishBatterylessEVFuseBudget(store *telemetry.Store, state *State, fuseMaxW float64) {
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	state.FuseEVMaxW, state.FuseSaturated = 0, false
	demand := math.Max(state.EVChargingW, scheduledEVPowerW(state))
	if meter == nil || demand <= 0 {
		return
	}
	cap := evHeadroomW(state.phaseImportCeilingW(fuseMaxW, meter), math.Max(meter.RawW, meter.SmoothedW), state.EVChargingW)
	state.FuseEVMaxW = math.Min(demand, cap)
	state.FuseSaturated = demand > cap
}
