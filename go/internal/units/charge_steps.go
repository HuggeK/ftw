package units

import "math"

// LimitChargeSteps owns its result. Invalid numbers stay visible to validation.
func LimitChargeSteps(steps []float64, maxW float64) ([]float64, bool) {
	out := make([]float64, 0, len(steps))
	limited := false
	seen := make(map[float64]bool, len(steps))
	for _, step := range steps {
		if maxW > 0 && !math.IsNaN(maxW) && !math.IsInf(maxW, 0) &&
			!math.IsNaN(step) && !math.IsInf(step, 0) && step > maxW {
			step = maxW
			limited = true
		}
		if !seen[step] {
			out = append(out, step)
			seen[step] = true
		}
	}
	return out, limited
}
