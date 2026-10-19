package config

import (
	"fmt"
	"math"
	"slices"

	"github.com/srcfl/ftw/go/internal/units"
)

func (c *Config) normalizeLoadpointSteps() error {
	// Own the slice before repairing it: config reload keeps the old snapshot.
	loadpoints := slices.Clone(c.Loadpoints)
	for i := range loadpoints {
		lp := &loadpoints[i]
		if math.IsNaN(lp.MaxChargeW) || math.IsInf(lp.MaxChargeW, 0) || lp.MaxChargeW < 0 {
			return fmt.Errorf("charger %q: max charging power must be finite and non-negative", lp.ID)
		}
		if math.IsNaN(lp.MinChargeW) || math.IsInf(lp.MinChargeW, 0) || lp.MinChargeW < 0 || lp.MinChargeW > lp.MaxChargeW {
			return fmt.Errorf("charger %q: min charging power must be between 0 and max charging power", lp.ID)
		}
		for _, step := range lp.AllowedStepsW {
			if math.IsNaN(step) || math.IsInf(step, 0) || step < 0 {
				return fmt.Errorf("charger %q: charging steps must be finite and non-negative", lp.ID)
			}
		}
		steps, limited := units.LimitChargeSteps(lp.AllowedStepsW, lp.MaxChargeW)
		lp.AllowedStepsW = steps
		if limited {
			warning := fmt.Sprintf("Charger %q: charging steps above the %g W limit were reduced to %g W.", lp.ID, lp.MaxChargeW, lp.MaxChargeW)
			if !slices.Contains(c.LoadpointWarnings, warning) {
				c.LoadpointWarnings = append(c.LoadpointWarnings, warning)
			}
		}
	}
	c.Loadpoints = loadpoints
	c.warnMissingLoadpointConnections()
	return nil
}
