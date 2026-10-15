package mpc

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Price optimisation must leave time for a simple plan when it runs out of
// time. The reserve pass below is linear in the number of intervals.
const coreRecoveryDPBudget = 2 * time.Second

// Only Core's own recovery builder may shorten the horizon. Worker plans
// must still match every requested interval. Every candidate passes replay.
func corePlanWithRecovery(ctx context.Context, slots []Slot, p Params) (Plan, []Slot, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, nil, err
	}
	slots, inputErr := planningInputPrefix(slots)
	if len(slots) == 0 {
		return Plan{}, nil, inputErr
	}
	if err := validatePlanningParams(p); err != nil {
		return Plan{}, nil, err
	}
	if err := coreDPModelError(p); err != nil {
		return Plan{}, nil, err
	}
	loads := p.activeLoadpoints()
	var dpErr error
	if len(loads) == 0 || loads[0].TargetSoC <= 0 {
		dpCtx, cancel := context.WithTimeout(ctx, coreRecoveryDPBudget)
		start := time.Now()
		plan, err := OptimizeContext(dpCtx, slots, p)
		cancel()
		if err == nil {
			err = ValidatePlan(slots, p, &plan)
		}
		if err == nil {
			plan.Solver = coreSolverInfo(p, msSince(start))
			if inputErr != nil {
				markShortenedPlan(&plan, slots, "Later planning inputs are invalid: "+inputErr.Error()+".")
			}
			return plan, slots, nil
		}
		dpErr = err
	}
	build := func(n int) (Plan, error) {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		plan, err := coreReservePlan(ctx, slots[:n], p)
		if err == nil {
			err = ValidatePlan(slots[:n], p, &plan)
		}
		return plan, err
	}
	plan, fullErr := build(len(slots))
	if fullErr == nil {
		if dpErr != nil {
			plan.Solver.Status, plan.Solver.Fallback = "fallback", true
			plan.Solver.FallbackReason = "Core price optimisation unavailable; using a reserve plan: " + dpErr.Error()
		}
		if inputErr != nil {
			markShortenedPlan(&plan, slots, "Later planning inputs are invalid: "+inputErr.Error()+".")
		}
		return plan, slots, nil
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, nil, err
	}
	// A feasible prefix does not need a later interval. Find the longest
	// prefix this reserve builder can replay within the physical limits.
	var best Plan
	count := 0
	for low, high := 1, len(slots)-1; low <= high; {
		mid := low + (high-low)/2
		candidate, err := build(mid)
		if err := ctx.Err(); err != nil {
			return Plan{}, nil, err
		}
		if err == nil {
			best, count = candidate, mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if count == 0 {
		return Plan{}, nil, fullErr
	}
	slog.Warn("mpc: forecast prevents a full reserve plan", "err", fullErr, "kept_slots", count, "requested_slots", len(slots))
	best.Solver.Status, best.Solver.Fallback = "fallback", true
	markShortenedPlan(&best, slots[:count], "A later forecast exceeds the site's power limits.")
	return best, slots[:count], nil
}

// Invalid future values must not discard earlier, independently valid inputs.
// A damaged timeline is ambiguous and stays an error for the whole request.
func planningInputPrefix(slots []Slot) ([]Slot, error) {
	if err := validateSlotChronology(slots); err != nil {
		return nil, err
	}
	validate := func(n int) error { return validatePlanningSlots(slots[:n]) }
	err := validate(len(slots))
	if err == nil {
		return slots, nil
	}
	count := 0
	for low, high := 1, len(slots)-1; low <= high; {
		mid := low + (high-low)/2
		if validate(mid) == nil {
			count, low = mid, mid+1
		} else {
			high = mid - 1
		}
	}
	return slots[:count], err
}

func markShortenedPlan(plan *Plan, slots []Slot, reason string) string {
	end, _ := checkedSlotEndMs(slots[len(slots)-1].StartMs, slots[len(slots)-1].LenMin)
	if plan.Solver != nil && plan.Solver.Fallback && plan.Solver.FallbackReason != "" {
		reason = plan.Solver.FallbackReason + "; " + reason
	}
	warning := fmt.Sprintf("Plan ends at %s. %s FTW will replan.", time.UnixMilli(end).Format("Mon 2 Jan 15:04 MST"), reason)
	if plan.Solver != nil && plan.Solver.Fallback {
		plan.Solver.FallbackReason = warning
	}
	return warning
}
