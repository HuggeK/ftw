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

// Keep a validated point plan before trying optional forecast caution. A risk
// pass may neither shorten its horizon nor worsen any departure inside it.
func planWithForecastMargin(ctx context.Context, point, risk []Slot, p Params, now func() time.Time, solve forecastSolve) (forecastChoice, error) {
	started := time.Now()
	plan, used, err := solve(ctx, point, p)
	if err == nil {
		err = ValidatePlan(used, p, &plan)
	}
	choice := forecastChoice{plan: plan, slots: used}
	if err != nil || len(point) == 0 || len(used) != len(point) || sameForecast(point, risk) {
		return choice, err
	}
	// ExecutionStartMs is the frozen energy snapshot, not the time left now.
	// The service clock also makes historical test snapshots deterministic.
	end, endErr := checkedSlotEndMs(point[0].StartMs, point[0].LenMin)
	if endErr != nil {
		return choice, endErr
	}
	remaining := time.UnixMilli(end).Sub(now()) - energyplanPublishMargin
	budget := min(forecastRecoveryBudget-time.Since(started), remaining)
	marginCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var candidate Plan
	var accepted []Slot
	err = marginCtx.Err()
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
	if err == nil && len(accepted) != len(used) {
		err = fmt.Errorf("forecast caution shortened the planning horizon")
	}
	if err == nil {
		err = ValidatePlan(accepted, p, &candidate)
	}
	if err == nil {
		err = validateMarginPointLoad(point, accepted, p, candidate)
	}
	if err == nil {
		err = preservesDepartureGoals(p, plan, candidate)
	}
	if err != nil {
		choice.marginErr = err
		choice.warning = "Forecast caution could not retain the point plan's limits and departure goals; using the point forecast: " + err.Error()
		return choice, nil
	}
	return forecastChoice{plan: candidate, slots: accepted}, nil
}

// Reuse Core replay with the same physical battery/EV choices and PV downside,
// but without optional extra house load. Recompute only balance and accounting
// on a copy: invented load cannot permit battery export or battery-fed charging.
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
	if err := ValidatePlan(slots, p, &plan); err != nil {
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
		// The plans have passed Core replay. Compare their allocated Wh, not
		// reported SoC or the notification's minimum-run shortfall threshold.
		missing := func(plan Plan) float64 {
			energy := lp.InitialSoC * lp.CapacityWh
			efficiency := lp.ChargeEfficiency
			if efficiency <= 0 {
				efficiency = .9
			}
			for _, a := range plan.Actions[:i+1] {
				power := a.LoadpointPowerW[lp.ID]
				if len(a.LoadpointPowerW) == 0 && lpIdx == 0 {
					power = a.LoadpointW
				}
				energy += power * a.DurationHours() * efficiency
			}
			return max(0, lp.TargetSoC*lp.CapacityWh-energy)
		}
		pointMissing, riskMissing := missing(point), missing(risk)
		// One Wh is the worker's deadline-energy comparison tolerance.
		if !finite(riskMissing) || riskMissing > pointMissing+1 {
			return fmt.Errorf("forecast caution increases %s departure shortfall from %.3f to %.3f Wh", lp.ID, pointMissing, riskMissing)
		}
	}
	return nil
}
