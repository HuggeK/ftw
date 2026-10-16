package loadpoint

import (
	"math"
	"sort"
	"time"
)

type fuseRequest struct {
	cfg      Config
	deadline time.Time
	wantW    float64
	liveW    float64
	goal     bool
}

// freezeFuseBudget divides one cap for the whole tick, before any command.
// A later callback or goal update cannot give a second car the first car's
// already offered share. Metered reductions earn new room only next tick.
func (c *Controller) freezeFuseBudget(now time.Time, samples map[string]EVSample) {
	c.tickFuseCaps = nil
	c.tickFusePhases = 0
	if c.fuseEVMax == nil {
		clear(c.fuseResumeTicks)
		return
	}
	total, ok := c.fuseEVMax()
	if !ok || total < 0 {
		clear(c.fuseResumeTicks)
		return
	}
	if !finite(total) {
		total = 0
	}
	d, _ := c.currentDirective(now)
	var requests []fuseRequest
	var liveTotal float64
	for _, cfg := range c.manager.Configs() {
		sample := samples[cfg.DriverName]
		st, _ := c.manager.State(cfg.ID)
		st.PluggedIn = sample.Connected && !sample.ConnectionUnknown
		request := fuseRequest{cfg: c.fuseConfig(now, cfg), deadline: st.TargetTime}
		if sample.Connected && finite(sample.PowerW) {
			request.liveW = math.Max(0, sample.PowerW)
			liveTotal += request.liveW
		}
		if st.PluggedIn && c.driverCanDispatch(cfg.DriverName) {
			if hold, held := c.GetManualHold(cfg.ID, now); held {
				request.wantW = clampManualPower(cfg, hold, c.siteFuse())
			} else {
				request.wantW, _, _ = c.computeCommandForBudget(now, cfg, sample.PowerW, false)
			}
			request.goal = request.wantW > 0 && !cfg.SurplusOnly
			if _, held := c.GetManualHold(cfg.ID, now); !held {
				c.batSoCArmedMu.Lock()
				armed := c.batSoCArmed[cfg.ID]
				c.batSoCArmedMu.Unlock()
				if cfg.SurplusOnly || armed || c.wakeKickActive(cfg.ID, now) || (st.FinishAtVehicleLimit && !st.GoalComplete) {
					request.wantW = cfg.MaxChargeW
				}
				if d.LoadpointFallback[cfg.ID] && sample.PowerUnavailable {
					request.wantW = 0
				}
			}
		}
		requests = append(requests, request)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		a, b := requests[i], requests[j]
		if a.goal != b.goal {
			return a.goal // Required charging precedes a speculative PV/wake offer.
		}
		if a.deadline.IsZero() != b.deadline.IsZero() {
			return !a.deadline.IsZero()
		}
		if !a.deadline.Equal(b.deadline) {
			return a.deadline.Before(b.deadline)
		}
		return a.cfg.ID < b.cfg.ID
	})
	c.tickFuseCaps = make(map[string]float64, len(requests))
	remaining, increase := total, math.Max(0, total-liveTotal)
	if phaseIncrease, known := c.sharedPhaseIncreaseW(now, requests, liveTotal); known {
		if phaseIncrease < 0 {
			total = math.Min(total, math.Max(0, liveTotal+phaseIncrease))
			remaining = total
		}
		increase = math.Max(0, math.Min(increase, phaseIncrease))
	}
	for _, request := range requests {
		target := floorChargeW(math.Min(request.wantW, remaining), request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		remaining -= target
		// Do not spend another car's curtailment before its meter confirms it.
		want := math.Min(target, request.liveW+increase)
		cap := floorChargeW(want, request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		cap = c.recoverFuseCap(request.cfg.ID, request.wantW, cap)
		c.tickFuseCaps[request.cfg.ID] = cap
		increase -= math.Max(0, cap-request.liveW)
	}
}

func (c *Controller) fuseConfig(now time.Time, cfg Config) Config {
	if hold, held := c.GetManualHold(cfg.ID, now); held {
		cfg.MinChargeW = manualMinChargeW(cfg, hold, c.siteFuse())
	}
	return cfg
}

func (c *Controller) recoverFuseCap(id string, wantW, capW float64) float64 {
	if wantW <= 0 {
		delete(c.fuseResumeTicks, id)
		return capW
	}
	if capW <= 0 {
		if c.fuseResumeTicks == nil {
			c.fuseResumeTicks = map[string]int{}
		}
		c.fuseResumeTicks[id] = 0
		return 0
	}
	if ready, paused := c.fuseResumeTicks[id]; paused {
		ready++
		if ready < fuseResumeSafeTicks {
			c.fuseResumeTicks[id] = ready
			return 0
		}
		delete(c.fuseResumeTicks, id)
	}
	return capW
}

// A new single-phase offer can land on the busiest phase. Do not spend a
// different car's still-measured draw to move power between phases.
func (c *Controller) sharedPhaseIncreaseW(now time.Time, requests []fuseRequest, liveTotal float64) (float64, bool) {
	if c.perPhaseMeterAmps == nil {
		return 0, false
	}
	l1, l2, l3, ok := c.perPhaseMeterAmps()
	site := c.siteFuse()
	if !ok || !finite(site.MaxAmps) || !finite(site.Voltage) || site.MaxAmps <= 0 || site.Voltage <= 0 {
		return 0, false
	}
	if !finite(l1) || !finite(l2) || !finite(l3) {
		return -liveTotal, true
	}
	phaseCount := site.Phases()
	unknownPhase := false
	for _, request := range requests {
		mode := c.phaseModeForFuse(now, request.cfg)
		if mode != "3p" {
			phaseCount = 1
			unknownPhase = true
		}
	}
	c.tickFusePhases = phaseCount
	worst := math.Max(math.Abs(l1), math.Max(math.Abs(l2), math.Abs(l3)))
	deltaA := site.MaxAmps - fusePhaseMarginA - worst
	if deltaA < 0 && unknownPhase {
		return -liveTotal, true
	}
	return deltaA * site.Voltage * float64(phaseCount), true
}

// Empty is the driver's locked-3p default unless the command branch selects
// auto or the surplus day lock selects 1p. Do not advance surplus hysteresis
// here: an enabled unlock may arm later in this tick, so retain its 1p bound.
func (c *Controller) phaseModeForFuse(now time.Time, cfg Config) string {
	sched, _ := c.manager.GetSchedule(cfg.ID)
	surplus := cfg.SurplusOnly
	if !sched.HasTarget() && sched.SurplusUnlockBatSoC > 0 && c.batSoC != nil &&
		!(c.tickDirectiveActive && c.tickDirectiveOK && c.tickDirective.LoadpointFallback[cfg.ID]) {
		surplus = true
	}
	mode := cfg.PhaseMode
	if hold, held := c.GetManualHold(cfg.ID, now); held {
		if hold.PhaseMode != "" {
			mode = hold.PhaseMode
		}
		if mode == "" && surplus {
			mode = "auto"
		}
	} else {
		// A surplus dwell can change while the offer is computed. Only a
		// fixed 3p mode earns three phases of new headroom in this guard.
		mode = resolvePhaseMode(mode, sched.HasTarget(), c.surplusLockedTo1P(cfg.ID), surplus, "")
	}
	if mode == "" {
		return "3p"
	}
	return mode
}

func (c *Controller) currentDirective(now time.Time) (Directive, bool) {
	if c.tickDirectiveActive {
		return c.tickDirective, c.tickDirectiveOK
	}
	if c.plan == nil {
		return Directive{}, false
	}
	return c.plan(now)
}

func (c *Controller) fuseCapFor(id string) (float64, bool) {
	if c.tickFuseCaps != nil {
		return c.tickFuseCaps[id], true
	}
	if c.tickDirectiveActive {
		return 0, false // The tick's one callback read found no constraint.
	}
	if c.fuseEVMax != nil {
		return c.fuseEVMax()
	}
	return 0, false
}
