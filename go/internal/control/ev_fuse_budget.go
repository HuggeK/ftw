package control

import (
	"encoding/json"
	"log/slog"
	"math"
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

// evSettle is how long one of our commands may take to reach the site meter.
const evSettle = loadpoint.CommandSettle

// evRestartDwell is how long the meter must keep showing room before an idle
// pool starts again. It stays above Easee's ~30 s contactor minimum.
const evRestartDwell = 35 * time.Second

// evPhaseMarginA is the working margin below the breaker on each phase. One
// amp on the pool's phases is also the rise step and the gap between the cut
// and rise thresholds.
const evPhaseMarginA = 1.0

// sentCommand is one of our own commands, never a device reading.
type sentCommand struct {
	at time.Time
	w  float64
}

// commandLog keeps a device's commands for evSettle, plus the one in force
// when that window opened. The meter may show any of them.
type commandLog []sentCommand

func (l commandLog) add(now time.Time, w float64) commandLog {
	l = append(l, sentCommand{now, w})
	i := 0
	for i+1 < len(l) && now.Sub(l[i+1].at) >= evSettle {
		i++
	}
	if i > 0 {
		return append(commandLog(nil), l[i:]...)
	}
	return l
}

// span returns the lowest, highest and latest command the meter may show.
// When the log is younger than evSettle, the meter may still show the time
// before our first command, which counts as 0.
func (l commandLog) span(now time.Time) (low, high, latest float64, ok bool) {
	for j, c := range l {
		if j+1 < len(l) && now.Sub(l[j+1].at) >= evSettle {
			continue // Replaced before the window opened.
		}
		if !ok || c.w < low {
			low = c.w
		}
		if !ok || c.w > high {
			high = c.w
		}
		latest, ok = c.w, true
	}
	if ok && now.Sub(l[0].at) < evSettle {
		low, high = math.Min(low, 0), math.Max(high, 0)
	}
	return low, high, latest, ok
}

// evMeterLimit is the managed EVs' common hard cap. It reads only the site
// meter, which measures the protected quantities, and our own commands. No
// charger, car or battery reading takes part in a cut, a rise or a start.
type evMeterLimit struct {
	set         bool
	capW        float64
	changedAt   time.Time // last cut, rise or idle level
	readingAt   time.Time // newest site reading seen
	roomSince   time.Time // first reading of the current run with room to start
	raised      bool
	raisedGridW float64 // site import when the last rise was sent
}

type evPool struct {
	offerW, wantW, minW, phases float64
	offerKnown                  bool
}

// EVFuseBudget returns the managed EVs' common hard cap, which the loadpoint
// controller divides between cars. Callers hold the control lock, and the
// shared freshness gate must already allow dispatch. An overage on the latest
// site reading cuts at once. Inside the limits the cap holds; it rises one
// step only on a fresh reading with room for two, once the meter has shown
// the last change. An idle pool starts only after the meter has kept showing
// room for its minimum plus a step. Each reading acts once, so a silent
// meter can never raise or restart a charge.
func EVFuseBudget(store *telemetry.Store, state *State, points []loadpoint.State, configs []loadpoint.Config, fuseMaxW float64) (float64, bool) {
	if store == nil || state == nil || !finiteFuseValue(fuseMaxW) || fuseMaxW <= 0 {
		return 0, true
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil {
		return 0, true
	}
	gridW := meter.RawW
	if !finiteFuseValue(gridW) {
		return 0, true
	}
	lim := &state.evLimit
	pool, plugged := evPoolFor(state, points, configs)
	if !plugged {
		*lim = evMeterLimit{}
		return 0, true
	}
	if pool.offerKnown {
		state.evOffers = state.evOffers.add(state.now(), pool.offerW)
	}
	if !lim.set || meter.UpdatedAt.After(lim.readingAt) {
		state.stepEVMeterLimit(meter, gridW, pool, fuseMaxW)
	}
	return lim.capW, true
}

func (s *State) stepEVMeterLimit(meter *telemetry.DerReading, gridW float64, pool evPool, fuseMaxW float64) {
	lim, now, at := &s.evLimit, s.now(), meter.UpdatedAt
	first := !lim.set
	gap := first || at.Sub(lim.readingAt) > evSettle
	lim.readingAt = at
	prev := math.Inf(1)
	if !first {
		prev = lim.capW
	}
	lim.set = true
	peakW := pool.offerW
	if _, high, _, ok := s.evOffers.span(now); ok {
		peakW = math.Max(peakW, high)
	}
	roomW, measured, worstA := s.evMeterRoomW(meter, gridW, pool.phases, fuseMaxW)
	if roomW < 0 {
		// The meter may still show any offer of the last evSettle. Taking
		// the overage from the highest cuts each measured watt once.
		lim.capW, lim.roomSince = math.Min(prev, math.Max(0, peakW+roomW)), time.Time{}
		if lim.capW < prev {
			lim.changedAt, lim.raised = now, false
			slog.Info("EV hard limit: site meter over limit", "grid_w", gridW, "worst_phase_a", worstA,
				"overage_w", -roomW, "offer_w", peakW, "cap_w", lim.capW)
		}
		return
	}
	// An increase needs a fresh reading and room left after our own battery
	// commands that the meter may not show yet.
	room := 0.0
	if _, fresh := qualifiedFuseObservation(meter, now); fresh && measured {
		room = math.Max(0, roomW-s.pendingBatteryCommandsW(now))
	}
	step := evPhaseMarginA * s.SiteFuseVoltage * pool.phases
	if pool.offerW <= 0 {
		lim.capW, lim.changedAt, lim.raised = 0, now, false
		if room < pool.minW+step {
			lim.roomSince = time.Time{}
			return
		}
		if lim.roomSince.IsZero() || gap {
			lim.roomSince = at
			if first {
				lim.roomSince = at.Add(-evRestartDwell) // No stop of ours to wait out.
			}
		}
		if at.Sub(lim.roomSince) >= evRestartDwell {
			lim.capW = room - step
		}
		return
	}
	// Keeping the current offers is never a cut. Headroom above them stays
	// within what this reading measured.
	lim.capW, lim.roomSince = math.Min(prev, peakW+room), time.Time{}
	if first {
		lim.changedAt = now
		return
	}
	// The meter shows the last change once import rose with the step, or
	// in any reading taken evSettle after it.
	seen := at.Sub(lim.changedAt) >= evSettle ||
		(lim.raised && at.After(lim.changedAt) && gridW >= lim.raisedGridW+step/2)
	if lim.capW < pool.wantW && room >= 2*step && seen {
		lim.capW += step
		lim.changedAt, lim.raised, lim.raisedGridW = now, true, gridW
	}
}

// evMeterRoomW is the measured room below the nearer limit, total import or
// worst phase, in EV watts. Negative is an overage. measured is false when
// the meter reports phase currents that cannot be read.
func (s *State) evMeterRoomW(meter *telemetry.DerReading, gridW, phases, fuseMaxW float64) (roomW float64, measured bool, worstA float64) {
	roomW = s.effectiveImportCeilingW(fuseMaxW) - gridW
	worstA, present, valid := worstPhaseA(meter, s)
	if !present {
		return roomW, true, 0
	}
	if !valid {
		return roomW, false, 0
	}
	return math.Min(roomW, (s.SiteFuseAmps-evPhaseMarginA-worstA)*s.SiteFuseVoltage*phases), true, worstA
}

// worstPhaseA reads the site's configured phases. present is false when the
// meter reports no phase currents; valid is false when they cannot be used.
func worstPhaseA(meter *telemetry.DerReading, state *State) (worstA float64, present, valid bool) {
	// DER data uses snake case; separate fields retain absent-vs-zero.
	var data struct {
		L1 *float64 `json:"l1_a"`
		L2 *float64 `json:"l2_a"`
		L3 *float64 `json:"l3_a"`
	}
	if json.Unmarshal(meter.Data, &data) != nil || (data.L1 == nil && data.L2 == nil && data.L3 == nil) {
		return 0, false, false
	}
	if !finiteFuseValue(state.SiteFuseAmps) || !finiteFuseValue(state.SiteFuseVoltage) || state.SiteFuseAmps <= 0 || state.SiteFuseVoltage <= 0 || state.SiteFusePhases < 1 || state.SiteFusePhases > 3 {
		return 0, true, false
	}
	for _, amps := range []*float64{data.L1, data.L2, data.L3}[:state.SiteFusePhases] {
		if amps == nil || !finiteFuseValue(*amps) {
			return 0, true, false
		}
		worstA = math.Max(worstA, math.Abs(*amps))
	}
	return worstA, true, true
}

// pendingBatteryCommandsW is battery increase we ordered that the meter may
// not show yet. Signed, so a stopped discharge counts too.
func (s *State) pendingBatteryCommandsW(now time.Time) float64 {
	var pending float64
	for _, log := range s.batteryCommands {
		if low, _, latest, ok := log.span(now); ok {
			pending += math.Max(0, latest-low)
		}
	}
	return pending
}

// evPoolFor sums the plugged managed chargers. Before the first command after
// a restart their offer is unknown, and the pool counts as idle.
func evPoolFor(state *State, points []loadpoint.State, configs []loadpoint.Config) (evPool, bool) {
	byID := make(map[string]loadpoint.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	pool := evPool{phases: 3, wantW: math.Inf(1), offerKnown: true}
	plugged := false
	for _, point := range points {
		if !point.PluggedIn {
			continue
		}
		plugged = true
		cfg := byID[point.ID]
		// Empty is the declared three-phase default. The controller's own
		// gate covers runtime auto/manual phase changes.
		if cfg.PhaseMode != "" && cfg.PhaseMode != "3p" {
			pool.phases = 1
		}
		if cfg.MinChargeW > 0 && (pool.minW == 0 || cfg.MinChargeW < pool.minW) {
			pool.minW = cfg.MinChargeW
		}
		pool.offerKnown = pool.offerKnown && point.CommandedKnown
	}
	if !plugged {
		return pool, false
	}
	if pool.offerKnown {
		pool.offerW = loadpoint.CommandedEVPowerW(points)
	}
	if state.evFuseDemandsSet {
		pool.wantW = 0
		for _, demand := range state.evFuseDemands {
			pool.wantW += demand.powerW
		}
	}
	if state.SiteFusePhases >= 1 && float64(state.SiteFusePhases) < pool.phases {
		pool.phases = float64(state.SiteFusePhases)
	}
	return pool, true
}

func qualifiedFuseObservation(r *telemetry.DerReading, now time.Time) (telemetry.ControlObservation, bool) {
	if r == nil || !finiteFuseValue(r.RawW) || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > telemetry.ControlPowerMaxAge(r.Data) {
		return telemetry.ControlObservation{}, false
	}
	p, ok := telemetry.ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	return p, ok && !p.Seen().After(now) && now.Sub(p.Seen()) <= telemetry.ControlWindowDuration
}

func publishBatterylessEVFuseBudget(store *telemetry.Store, state *State, fuseMaxW float64) {
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	state.FuseEVMaxW, state.FuseSaturated = 0, false
	evInMeterW := state.evInMeterW()
	demand := math.Max(evInMeterW, scheduledEVPowerW(state))
	if meter == nil || demand <= 0 {
		return
	}
	cap := evHeadroomW(state.phaseImportCeilingW(fuseMaxW, meter), math.Max(meter.RawW, meter.SmoothedW), evInMeterW)
	state.FuseEVMaxW = math.Min(demand, cap)
	state.FuseSaturated = demand > cap
}
