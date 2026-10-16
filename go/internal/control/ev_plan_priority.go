package control

import "math"

// The active plan contains EV energy only for a connected scheduled goal.
// Keep its demand available while the charger ramps up, even before it draws.
// A duty-cycle pulse spends a small Wh budget at a legal on-power. Fuse
// sharing must reserve that on-power, not Wh/slot, or the battery can take
// headroom the charger is about to use.
func scheduledEVPowerW(state *State) float64 {
	if state == nil {
		return 0
	}
	commanded := state.EVCommandedW
	if math.IsNaN(commanded) || math.IsInf(commanded, 0) || commanded < 0 {
		commanded = 0
	}
	if !state.Mode.IsPlannerMode() {
		return commanded
	}
	dir, ok := planDirectiveForIntent(state)
	// EV reserve intent remains available while the battery controller
	// honestly follows its missing-plan self-consumption path.
	if !ok && state.controlPlanSnapshot.directiveOK && state.controlPlanSnapshot.directive.EVOnly {
		dir, ok = state.controlPlanSnapshot.directive, true
	}
	if !ok || state.now().Before(dir.SlotStart) || !state.now().Before(dir.SlotEnd) {
		return commanded
	}
	hours := dir.SlotEnd.Sub(dir.SlotStart).Hours()
	if hours <= 0 {
		return commanded
	}
	var reserved float64
	for id, wh := range dir.LoadpointEnergyWh {
		if math.IsNaN(wh) || math.IsInf(wh, 0) || wh < 0 {
			return commanded
		}
		if wh <= 0 {
			continue
		}
		if peak, ok := dir.LoadpointMaxPowerW[id]; ok {
			if math.IsNaN(peak) || math.IsInf(peak, 0) || peak < 0 {
				return commanded
			}
			if peak > 0 {
				reserved += peak
				continue
			}
		}
		reserved += wh / hours
	}
	return math.Max(commanded, reserved)
}
