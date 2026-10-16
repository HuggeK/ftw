package mpc

import (
	"context"
	"fmt"
	"time"
)

type forecastSolve func(context.Context, []Slot, Params) (Plan, []Slot, error)

type forecastChoice struct {
	plan      Plan
	slots     []Slot
	warning   string
	marginErr error
	// withoutCaution is set when the plan ignores a caution forecast that
	// differed from the point forecast.
	withoutCaution bool
}

func validateForecastCaution(p Params) error {
	for _, value := range []struct {
		name string
		v    float64
	}{
		{"pv_uncertainty_w", p.PVUncertaintyW},
		{"pv_relative_uncertainty", p.PVRelativeUncertainty},
		{"pv_forecast_safety_k", p.PVForecastSafetyK},
	} {
		if err := requireNonNegativePlanningValue(value.name, value.v); err != nil {
			return err
		}
	}
	return nil
}

// Keep a validated point plan whenever optional forecast caution fails. A
// risk pass may neither shorten its horizon nor worsen any departure inside it.
//
// With time for both passes, caution goes first. A full cautious plan that
// misses no departure leaves nothing for a point plan to improve on, so it is
// published after one solve rather than two: the worker spends its whole
// budget on a plan with a car, and the second pass doubled plug-in-to-plan.
func planWithForecastMargin(ctx context.Context, point, risk []Slot, p Params, now func() time.Time, solve forecastSolve) (forecastChoice, error) {
	started := time.Now()
	solvePoint := func() (forecastChoice, error) {
		plan, used, err := solve(ctx, point, p)
		if err == nil {
			err = ValidatePlan(used, p, &plan)
		}
		return forecastChoice{plan: plan, slots: used}, err
	}
	if len(point) == 0 || sameForecast(point, risk) {
		return solvePoint()
	}
	// ExecutionStartMs is the frozen energy snapshot, not the time left now.
	// The service clock also makes historical test snapshots deterministic.
	end, endErr := checkedSlotEndMs(point[0].StartMs, point[0].LenMin)
	if endErr != nil {
		choice, err := solvePoint()
		if err == nil {
			err = endErr
		}
		return choice, err
	}
	remaining := func() time.Duration { return time.UnixMilli(end).Sub(now()) - energyplanPublishMargin }
	available := remaining()
	if deadline, ok := ctx.Deadline(); ok {
		available = min(available, time.Until(deadline))
	}
	cautionFirst := available >= forecastRecoveryBudget
	var choice forecastChoice
	if !cautionFirst {
		var err error
		if choice, err = solvePoint(); err != nil || len(choice.slots) != len(point) {
			// A shortened point plan never keeps caution.
			choice.withoutCaution = err == nil
			return choice, err
		}
	}
	// The point solve took time; caution must still end inside the slot.
	budget := min(forecastRecoveryBudget-time.Since(started), remaining())
	if cautionFirst {
		// Leave the point plan the largest worker budget in case it is needed.
		budget = min(budget, available-energyplanFleetBudget)
	}
	marginCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var candidate Plan
	var accepted []Slot
	err := marginCtx.Err()
	if err == nil && len(risk) != len(point) {
		err = fmt.Errorf("forecast caution changed the planning horizon")
	}
	if err == nil {
		for i, slot := range risk {
			slot.PVW, slot.LoadW = point[i].PVW, point[i].LoadW
			if slot != point[i] {
				err = fmt.Errorf("forecast caution changed slot %d market data, timeline or site limits", i)
				break
			}
		}
	}
	if err == nil {
		candidate, accepted, err = solve(marginCtx, risk, p)
	}
	// Both orders keep only a full point plan, so caution must be full too.
	if err == nil && len(accepted) != len(point) {
		err = fmt.Errorf("forecast caution shortened the planning horizon")
	}
	if err == nil {
		err = ValidatePlan(accepted, p, &candidate)
	}
	if err == nil {
		err = validateMarginPointLoad(point, accepted, p, candidate)
	}
	if cautionFirst {
		if err == nil && missesNoDeparture(p, candidate) {
			return forecastChoice{plan: candidate, slots: accepted}, nil
		}
		var pointErr error
		if choice, pointErr = solvePoint(); pointErr != nil || len(choice.slots) != len(point) {
			choice.withoutCaution = pointErr == nil
			return choice, pointErr
		}
	}
	if err == nil {
		err = preservesDepartureGoals(p, choice.plan, candidate)
	}
	if err != nil {
		choice.marginErr, choice.withoutCaution = err, true
		choice.warning = "Forecast caution could not retain the point plan's limits and departure goals; using the point forecast: " + err.Error()
		return choice, nil
	}
	return forecastChoice{plan: candidate, slots: accepted}, nil
}

// Reuse Core replay with the same physical battery/EV choices and PV downside,
// but without optional extra house load. Recompute only balance and accounting
// on a copy: invented load cannot permit battery export the mode forbids, or
// break a grid limit.
//
// The vehicle rules stay with the cautious plan's own inputs, where it has
// already passed them. Load that does not show up cannot make the battery
// feed a protected car: with battery_covers_ev off, dispatch stops discharge
// at the house. With it on, the owner lets the battery feed the car, and the
// battery energy the house does not use is export, which the mode judges here.
func validateMarginPointLoad(point, risk []Slot, p Params, plan Plan) error {
	slots := append([]Slot(nil), point...)
	plan.Actions = append([]Action(nil), plan.Actions...)
	plan.TotalCostOre = 0
	for i := range slots {
		a := &plan.Actions[i]
		a.GridW += point[i].LoadW - risk[i].LoadW
		slots[i].PVW = risk[i].PVW
		a.LoadW = point[i].LoadW
		a.CostOre = reserveSlotCost(slots[i], p, *a)
		plan.TotalCostOre += a.CostOre
	}
	if err := validatePlan(slots, p, &plan, false); err != nil {
		return fmt.Errorf("forecast caution requires extra house load to obey site policy: %w", err)
	}
	return nil
}

func preservesDepartureGoals(p Params, point, risk Plan) error {
	for lpIdx, lp := range p.activeLoadpoints() {
		i := lp.TargetSlotIdx
		if lp.TargetSoC <= 0 || i < 0 || i >= len(point.Actions) {
			continue
		}
		if i >= len(risk.Actions) {
			return fmt.Errorf("forecast caution omits departure for %s", lp.ID)
		}
		pointMissing, riskMissing := departureShortfallWh(point, lp, lpIdx), departureShortfallWh(risk, lp, lpIdx)
		// One Wh is the worker's deadline-energy comparison tolerance.
		if !finite(riskMissing) || riskMissing > pointMissing+1 {
			return fmt.Errorf("forecast caution increases %s departure shortfall from %.3f to %.3f Wh", lp.ID, pointMissing, riskMissing)
		}
	}
	return nil
}

// missesNoDeparture holds when the plan reaches every departure goal inside it
// within the same tolerance, so preservesDepartureGoals accepts it against any
// point plan.
func missesNoDeparture(p Params, plan Plan) bool {
	for lpIdx, lp := range p.activeLoadpoints() {
		i := lp.TargetSlotIdx
		if lp.TargetSoC <= 0 || i < 0 || i >= len(plan.Actions) {
			continue
		}
		if missing := departureShortfallWh(plan, lp, lpIdx); !finite(missing) || missing > 1 {
			return false
		}
	}
	return true
}

// The plans have passed Core replay. Compare their allocated Wh, not reported
// SoC or the notification's minimum-run shortfall threshold.
func departureShortfallWh(plan Plan, lp *LoadpointSpec, lpIdx int) float64 {
	energy := lp.InitialSoC * lp.CapacityWh
	efficiency := lp.ChargeEfficiency
	if efficiency <= 0 {
		efficiency = .9
	}
	for _, a := range plan.Actions[:lp.TargetSlotIdx+1] {
		power := a.LoadpointPowerW[lp.ID]
		if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
			power = a.LoadpointW
		}
		energy += power * a.DurationHours() * efficiency
	}
	return max(0, lp.TargetSoC*lp.CapacityWh-energy)
}
