package control

import (
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
	raisedGridW float64            // site import when the last rise was sent
	overSince   time.Time          // first reading of a run over the hour's import limit
	overOffers  map[string]float64 // each charger's offer when that run began
	idleWh      float64            // import since roomSince, for the ceiling's start test
}

type evPool struct {
	offerW, wantW, minW, phases float64
	offerKnown                  bool
	offers                      map[string]float64 // per charger driver
}

// EVFuseBudget returns the managed EVs' common hard cap, which the loadpoint
// controller divides between cars. Callers hold the control lock, and the
// shared freshness gate must already allow dispatch. A fuse overage in total
// import cuts at once; a phase overage cuts once that phase's heat budget is
// spent, or at once above fuseInstantFactor. Otherwise the cap holds; it
// rises one step only on a fresh reading with room for two, once the meter
// has shown the last change. An idle pool starts only after the meter has
// kept showing room for its minimum plus a step. Each reading acts once, so
// a silent meter can never raise or restart a charge.
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
	state.stepFuseHeat(meter)
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
	first, prevAt := !lim.set, lim.readingAt
	gap := first || at.Sub(prevAt) > evSettle
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
	r := s.evMeterRoom(meter, gridW, pool.phases, fuseMaxW)
	step := evPhaseMarginA * s.SiteFuseVoltage * pool.phases
	// The fuse cuts on every reading, by its heat budget where the meter
	// reports phase currents. The hour's budget absorbs a short overage of
	// less than a step: it cuts once the overage has lasted evSettle, so one
	// reading over the ceiling changes nothing.
	cutW := r.fuseCutW
	if r.limitW >= 0 {
		lim.overSince = time.Time{}
	} else {
		if lim.overSince.IsZero() || gap {
			lim.overSince, lim.overOffers = at, pool.offers
		}
		if !r.hourly || gap || -r.limitW >= step || at.Sub(lim.overSince) >= evSettle {
			cutW = math.Min(cutW, r.limitW)
		}
	}
	if cutW < 0 {
		// The meter may still show any offer of the last evSettle. Taking
		// the overage from the highest cuts each measured watt once.
		lim.capW, lim.roomSince = math.Min(prev, math.Max(0, peakW+cutW)), time.Time{}
		if lim.capW < prev {
			lim.changedAt, lim.raised = now, false
			slog.Info("EV hard limit: site meter over limit", "grid_w", gridW, "worst_phase_a", r.worstA,
				"fuse_heat_a", s.fuseHeat.worstA(), "import_limit_w", s.effectiveImportCeilingW(fuseMaxW),
				"overage_w", -cutW, "offer_w", peakW, "cap_w", lim.capW)
		}
		return
	}
	// An increase needs a fresh reading and room below the level increases
	// may reach, left after our own battery commands that the meter may not
	// show yet. Between that level and the import limit the cap holds.
	pending := s.pendingBatteryCommandsW(now)
	_, fresh := qualifiedFuseObservation(meter, now)
	fresh = fresh && r.measured
	room := 0.0
	if fresh {
		room = math.Max(0, math.Min(r.fuseW, r.riseW)-pending)
	}
	if pool.offerW <= 0 {
		s.stepEVIdleStart(r, gridW, pool, first, gap, fresh, at, prevAt, pending, step)
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

// stepEVIdleStart starts an idle pool once the meter has kept showing room
// for evRestartDwell: room on every reading for its minimum plus a step
// below the fuse. Under a measured hour, the ceiling's part is judged on the
// dwell's mean import, the house's load rather than one reading of it, and
// the minimum needs no step of margin there: a short overage waits.
func (s *State) stepEVIdleStart(r evMeterRoom, gridW float64, pool evPool, first, gap, fresh bool, at, prevAt time.Time, pending, step float64) {
	lim := &s.evLimit
	lim.capW, lim.changedAt, lim.raised = 0, s.now(), false
	perReadingW := r.fuseW
	if !r.hourly {
		perReadingW = math.Min(perReadingW, r.riseW)
	}
	room := 0.0
	if fresh {
		room = math.Max(0, perReadingW-pending)
	}
	if room < pool.minW+step {
		lim.roomSince = time.Time{}
		return
	}
	switch {
	case first:
		// No stop of ours to wait out.
		lim.roomSince, lim.idleWh = at.Add(-evRestartDwell), math.Max(0, gridW)*evRestartDwell.Hours()
	case lim.roomSince.IsZero() || gap:
		lim.roomSince, lim.idleWh = at, 0
	default:
		lim.idleWh += math.Max(0, gridW) * at.Sub(prevAt).Hours()
	}
	dwell := at.Sub(lim.roomSince)
	if dwell < evRestartDwell {
		return
	}
	lim.capW = room - step
	if !r.hourly {
		return
	}
	ceilingRoomW := r.startLevelW - lim.idleWh/dwell.Hours() - pending
	if ceilingRoomW < pool.minW {
		lim.capW, lim.roomSince, lim.idleWh = 0, at, 0 // Judge the next dwell.
		return
	}
	lim.capW = math.Min(lim.capW, ceilingRoomW)
}

// evMeterRoom is the measured room in EV watts. The fuse's part holds on
// every reading. The import ceiling's parts are +Inf while it is off.
type evMeterRoom struct {
	fuseW       float64 // below the fuse less its margin and the worst phase, read or heat; rises and starts
	fuseCutW    float64 // what the fuse needs cut now, negative; see fuseHeat
	limitW      float64 // below the hour's import limit; negative is an overage
	riseW       float64 // below the level an increase may reach
	startLevelW float64 // import an idle start may reach: the rise level, at most the ceiling
	hourly      bool    // the hour's import is measured, so a short overage may wait
	measured    bool    // false when the meter reports phase currents that cannot be read
	worstA      float64
}

func (s *State) evMeterRoom(meter *telemetry.DerReading, gridW, phases, fuseMaxW float64) evMeterRoom {
	inf, totalW := math.Inf(1), math.Max(0, fuseMaxW-s.fuseSafetyMarginW())-gridW
	r := evMeterRoom{fuseW: totalW, fuseCutW: totalW, limitW: inf, riseW: inf, startLevelW: inf}
	if limitW, riseW, ok := s.importHourLimitsW(); ok {
		r.limitW, r.riseW = limitW-gridW, riseW-gridW
		// A car that could run only on one hour's slack would stop when
		// the next hour starts at the ceiling.
		r.startLevelW = math.Min(riseW, s.PeakImportCeilingW)
		r.hourly = s.importHourMeasured()
	}
	amps, present, valid := phaseCurrentsA(meter, s)
	if present && !valid {
		return r
	}
	r.measured = true
	if present {
		limitA := s.SiteFuseAmps - evPhaseMarginA
		for _, a := range amps {
			r.worstA = math.Max(r.worstA, a)
		}
		r.fuseW = math.Min(r.fuseW, (limitA-s.fuseHeat.levelA(amps))*s.SiteFuseVoltage*phases)
		// The phases measure the fuse itself; total import stands in only without them.
		r.fuseCutW = -s.fuseHeat.overA(amps, limitA, fuseInstantFactor*s.SiteFuseAmps) * s.SiteFuseVoltage * phases
	}
	return r
}

// worstPhaseA reads the site's configured phases. present is false when the
// meter reports no phase currents; valid is false when they cannot be used.
func worstPhaseA(meter *telemetry.DerReading, state *State) (worstA float64, present, valid bool) {
	amps, present, valid := phaseCurrentsA(meter, state)
	for _, a := range amps {
		worstA = math.Max(worstA, a)
	}
	return worstA, present, valid
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

// SetEVCommanded stores this tick's final charger offers, after every
// charger clamp, in total and per driver.
func (s *State) SetEVCommanded(points []loadpoint.State) {
	s.EVCommandedW = loadpoint.CommandedEVPowerW(points)
	s.evCommanded = loadpoint.CommandedEVPowerByDriver(points)
}

// pendingEVCutW is the EV reduction we sent during the current run over the
// hour's import limit: for each charger still plugged, its offer when the
// run began less its offer this tick. A car that leaves drops out, since no
// cut of ours removed it; a car that joins has no start offer. Our own
// commands only. A cut that has reached the meter ends the run, and no run
// waits longer than evSettle, so a car that draws less than its offer cannot
// hold an overage through a second cut.
func (s *State) pendingEVCutW() float64 {
	lim := &s.evLimit
	if lim.overSince.IsZero() || s.now().Sub(lim.overSince) >= evSettle {
		return 0
	}
	var cutW float64
	for driver, startW := range lim.overOffers {
		if nowW, plugged := s.evCommanded[driver]; plugged {
			cutW += math.Max(0, startW-math.Max(0, nowW))
		}
	}
	return cutW
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
	pool.offers = loadpoint.CommandedEVPowerByDriver(points)
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
