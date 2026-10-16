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
	if c.fuseEVMax == nil {
		return
	}
	total, ok := c.fuseEVMax()
	if !ok || total < 0 {
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
		request := fuseRequest{cfg: cfg, deadline: st.TargetTime}
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
		c.tickFuseCaps[request.cfg.ID] = cap
		increase -= math.Max(0, cap-request.liveW)
	}
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
		return 0, true
	}
	phaseCount := site.Phases()
	unknownPhase := false
	for _, request := range requests {
		mode := request.cfg.PhaseMode
		if hold, ok := c.GetManualHold(request.cfg.ID, now); ok && hold.PhaseMode != "" {
			mode = hold.PhaseMode
		}
		if mode == "1p" || mode == "auto" {
			phaseCount = 1
			unknownPhase = true
		}
	}
	worst := math.Max(math.Abs(l1), math.Max(math.Abs(l2), math.Abs(l3)))
	deltaA := site.MaxAmps - fusePhaseMarginA - worst
	if deltaA < 0 && unknownPhase {
		return -liveTotal, true
	}
	return deltaA * site.Voltage * float64(phaseCount), true
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
	if c.fuseEVMax != nil {
		return c.fuseEVMax()
	}
	return 0, false
}
