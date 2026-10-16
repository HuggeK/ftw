package loadpoint

import (
	"math"
)

// DeadlineNeed is the remaining AC energy and legal on-power of a saved goal.
// It does not promise future site capacity or authorize a hardware command.
func DeadlineNeed(st State) (energyWh, powerW float64, ok bool) {
	if !st.PluggedIn || st.ChargingDeclined || st.GoalComplete || st.TargetTime.IsZero() ||
		!finite(st.CurrentSoC) || st.CurrentSoC < 0 || st.CurrentSoC > 1 ||
		!finite(st.TargetSoC) || st.TargetSoC < 0 || st.TargetSoC > 1 ||
		!finite(st.VehicleCapacityWh) || st.VehicleCapacityWh <= 0 ||
		!finite(st.MinChargeW) || !finite(st.MaxChargeW) || st.MinChargeW <= 0 || st.MaxChargeW < st.MinChargeW {
		return 0, 0, false
	}
	target := st.TargetSoC
	if st.FinishAtVehicleLimit {
		target = 1 // The controller still stops at the car's own completion.
	}
	energyWh = (target - st.CurrentSoC) * st.VehicleCapacityWh / DefaultChargeEfficiency
	if energyWh <= 0 || !finite(energyWh) {
		return 0, 0, false
	}
	for _, step := range st.AllowedStepsW {
		if !finite(step) || step < 0 || step > st.MaxChargeW {
			return 0, 0, false
		}
		if step >= st.MinChargeW {
			powerW = math.Max(powerW, step)
		}
	}
	if len(st.AllowedStepsW) == 0 {
		powerW = st.MaxChargeW
	}
	return energyWh, powerW, powerW >= st.MinChargeW
}

// CommandedEVPowerW uses the actual offers after every charger clamp. A
// charger need not have started drawing for its offer to occupy the fuse.
func CommandedEVPowerW(states []State) float64 {
	byDriver := make(map[string]float64, len(states))
	for _, st := range states {
		if st.PluggedIn && st.CommandedKnown && finite(st.CommandedW) && st.CommandedW > byDriver[st.DriverName] {
			byDriver[st.DriverName] = st.CommandedW
		}
	}
	var total float64
	for _, w := range byDriver {
		total += w
	}
	return total
}
