package mpc

import (
	"fmt"
	"log/slog"

	"github.com/srcfl/ftw/go/internal/units"
)

func (s *Service) failedReplan(request replanRequest, message string) *Plan {
	s.mu.Lock()
	if request.generation == s.latestReplanGeneration && !request.wasCanceledByService() {
		s.lastPlanningError = message
	}
	plan := s.last
	s.mu.Unlock()
	return plan
}

func limitPlanningLoadpointSteps(p *Params) []string {
	var warnings []string
	copySpec := func(lp *LoadpointSpec) *LoadpointSpec {
		if lp == nil {
			return nil
		}
		copy := *lp
		steps, limited := units.LimitChargeSteps(lp.AllowedStepsW, lp.MaxChargeW)
		copy.AllowedStepsW = steps
		if limited {
			warning := fmt.Sprintf("Charger %q: charging steps above the %g W limit were reduced to %g W.", lp.ID, lp.MaxChargeW, lp.MaxChargeW)
			warnings = append(warnings, warning)
			slog.Warn("mpc: charging steps limited to max charging power", "charger", lp.ID, "max_charge_w", lp.MaxChargeW)
		}
		return &copy
	}
	// The legacy single-loadpoint fallback must use the same corrected physics.
	original := p.Loadpoint
	p.Loadpoints = append([]*LoadpointSpec(nil), p.Loadpoints...)
	for i, lp := range p.Loadpoints {
		p.Loadpoints[i] = copySpec(lp)
		if lp == original {
			p.Loadpoint = p.Loadpoints[i]
			original = nil
		}
	}
	if original != nil {
		p.Loadpoint = copySpec(original)
	}
	return warnings
}
