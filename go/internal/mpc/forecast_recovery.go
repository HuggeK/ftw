package mpc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Two bounded worker passes may each need the fleet budget.
const forecastRecoveryBudget = 12 * time.Second

const forecastPriorWarning = "Load forecast cannot be served within site limits; using the configured load prior"

// Only a well-formed infeasibility result justifies changing forecast
// assumptions. Timeouts, broken replies and invalid hardware settings do not.
func forecastInfeasible(err error) bool {
	var reply *externalError
	return errors.As(err, &reply) && reply.Code == "infeasible"
}

func sameForecast(a, b []Slot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PVW != b[i].PVW || a[i].LoadW != b[i].LoadW {
			return false
		}
	}
	return true
}

// A configured prior is an explicit degraded forecast, never a new physical
// limit or a claim about measured demand. Keep qualified current house load.
func priorForecast(base []Slot, loadW float64, currentMeasured bool) []Slot {
	if !finite(loadW) || loadW < 0 {
		return nil
	}
	out := append([]Slot(nil), base...)
	for i := range out {
		if i != 0 || !currentMeasured {
			out[i].LoadW = loadW
		}
	}
	if validatePlanningSlots(out) != nil {
		return nil
	}
	return out
}

type forecastAttempt struct {
	slots  []Slot
	base   []Slot
	reason string
}

// One point-forecast retry and one prior retry at most. The same optimiser
// decides feasibility and price choice; Core does not duplicate its physics.
func forecastRecoveryAttempts(planning, base []Slot, priorW float64, currentMeasured bool) []forecastAttempt {
	attempts := []forecastAttempt{}
	if !sameForecast(planning, base) && validatePlanningSlots(base) == nil {
		attempts = append(attempts, forecastAttempt{base, base, "Forecast risk margin cannot be served within site limits; using the point forecast"})
	}
	prior := priorForecast(base, priorW, currentMeasured)
	if prior != nil && !sameForecast(planning, prior) && !sameForecast(base, prior) {
		attempts = append(attempts, forecastAttempt{prior, prior, forecastPriorWarning})
	}
	return attempts
}

// Non-finite or negative model load is missing data, not zero consumption.
// Keep the raw values for diagnosis; use an independent numeric forecast.
func recoverInvalidForecastLoads(slots, legacy []Slot, priorW float64) ([]Slot, error) {
	out := append([]Slot(nil), slots...)
	count := 0
	for i := range out {
		if finite(out[i].LoadW) && out[i].LoadW >= 0 {
			continue
		}
		replacement := priorW
		if i < len(legacy) && finite(legacy[i].LoadW) && legacy[i].LoadW >= 0 {
			replacement = legacy[i].LoadW
		}
		if finite(replacement) && replacement >= 0 {
			out[i].LoadW = replacement
			count++
		}
	}
	if count > 0 {
		return out, fmt.Errorf("Rejected %d invalid load forecasts; using legacy forecasts or the configured load prior", count)
	}
	return out, nil
}

// Core keeps the point plan before optional caution or a degraded load prior.
// A prior may extend a safe prefix, but cannot replace a complete point plan.
func corePlanWithForecastRecovery(ctx context.Context, risk, point []Slot, p Params, priorW float64, currentMeasured bool, now func() time.Time) (forecastChoice, error) {
	choice, err := planWithForecastMargin(ctx, point, risk, p, now, corePlanWithRecovery)
	if err == nil && len(choice.slots) == len(point) {
		return choice, nil
	}
	prior := priorForecast(point, priorW, currentMeasured)
	if prior == nil || sameForecast(point, prior) || ctx.Err() != nil {
		return choice, err
	}
	candidate, accepted, retryErr := corePlanWithRecovery(ctx, prior, p)
	if retryErr == nil && len(accepted) > len(choice.slots) {
		return forecastChoice{plan: candidate, slots: accepted, warning: forecastPriorWarning,
			withoutCaution: !sameForecast(point, risk)}, nil
	}
	return choice, err
}
