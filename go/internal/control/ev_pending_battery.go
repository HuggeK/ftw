package control

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type pendingBatterySeries struct {
	upperW, fallbackW float64
	current           telemetry.ControlObservation
	fresh             bool
	points            []telemetry.ControlObservation
}

// batteryOrderUpperW keeps the same order bound before EV and battery sends.
// PrevTargets covers a computed offer before the driver's call begins; once
// command evidence exists it owns the outstanding order, including releases.
func batteryOrderUpperW(store *telemetry.Store, state *State, driver string) (float64, bool) {
	offer, offered := state.PrevTargets[driver]
	if !offered {
		return store.BatteryCommandUpperW(driver)
	}
	var at time.Time
	if state.LastDispatch != nil {
		at = *state.LastDispatch
	}
	if upper, sent := store.BatteryCommandUpperW(driver, telemetry.CommandPoint{PowerW: offer, At: at}); sent {
		return upper, true
	}
	return offer, offered
}

func pendingBatterySeriesForFuse(store *telemetry.Store, state *State, windows map[string]telemetry.ControlBaseline, now time.Time) []pendingBatterySeries {
	drivers := make(map[string]bool)
	for driver := range state.DriverLimits {
		drivers[driver] = true
	}
	for _, reading := range store.ReadingsByType(telemetry.DerBattery) {
		drivers[reading.Driver] = true
	}
	var series []pendingBatterySeries
	for driver := range drivers {
		upper, known := batteryOrderUpperW(store, state, driver)
		reading := store.Get(driver, telemetry.DerBattery)
		current, fresh := qualifiedFuseObservation(reading, now)
		if fresh {
			if known {
				upper = math.Max(upper, current.PowerW)
			} else {
				upper = current.PowerW
			}
		} else if !known {
			continue
		}
		// A measured increase can precede the site poll even when FTW
		// sent no increase. Keep it in the same AC domain as the baseline.
		// Missing battery data cannot promise a positive baseline. The
		// configured discharge bound also covers an unobserved discharge
		// that a pending zero/charge order could stop.
		series = append(series, pendingBatterySeries{upperW: upper, fallbackW: -state.DriverLimits[driver].dischargeCap(),
			current: current, fresh: fresh, points: append(windows[driver+":"+telemetry.DerBattery.String()].Points, current)})
	}
	return series
}

// Reserve orders against the same site observation as the EV replacement
// credit. Signed subtraction also reserves a loss of existing discharge.
func pendingBatteryIncreaseAt(series []pendingBatterySeries, siteAt time.Time) (reserve float64, coherent bool) {
	coherent = true
	for _, battery := range series {
		baseline, known := battery.fallbackW, false
		if battery.fresh {
			if value, ok := bracketedBatteryPower(battery.points, siteAt, battery.current.PowerW); ok {
				baseline, known = value, true
			}
		}
		coherent = coherent && known
		if !finiteFuseValue(battery.upperW) || !finiteFuseValue(baseline) {
			return math.Inf(1), false
		}
		reserve += math.Max(0, battery.upperW-baseline)
	}
	return reserve, coherent
}

func pendingBatteryIncreaseAtLatestSite(series []pendingBatterySeries, siteAt time.Time) float64 {
	var reserve float64
	for _, battery := range series {
		baseline := battery.fallbackW
		if battery.fresh {
			baseline = batteryPowerAtLatestSite(battery.points, siteAt, battery.current, battery.fallbackW)
		}
		if !finiteFuseValue(battery.upperW) || !finiteFuseValue(baseline) {
			return math.Inf(1)
		}
		reserve += math.Max(0, battery.upperW-baseline)
	}
	return reserve
}
