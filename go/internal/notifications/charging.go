package notifications

import "time"

// ConnectedSettle is how long after a plug-in the box decides whether to
// tell anyone. A car that reports its own level usually does so within it,
// and a car whose level and goal are both known needs no question.
const ConnectedSettle = 90 * time.Second

// ChargingFacts is what the plug-in rule needs to know about a loadpoint.
type ChargingFacts struct {
	Plugged bool
	// LevelKnown is true when the car's level came from the car or from
	// someone who set it, not from the plug-in assumption.
	LevelKnown bool
	// HasGoal is true when a target level is set.
	HasGoal bool
}

// SetChargingFacts wires what the plug-in rule reads. Nil tells every
// plug-in, as before.
func (s *Service) SetChargingFacts(f func(loadpointID string) (ChargingFacts, bool)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.charging = f
	s.mu.Unlock()
}

// handleConnected tells a plug-in only when the box needs the household:
// the car's level is an assumption, or no goal is set. It decides after
// ConnectedSettle, and says nothing if the cable came out meanwhile.
func (s *Service) handleConnected(loadpointID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	facts, settle := s.charging, s.connectedSettle
	s.mu.Unlock()
	if facts != nil {
		if settle > 0 {
			time.Sleep(settle)
		}
		f, ok := facts(loadpointID)
		if !ok || !f.Plugged || (f.LevelKnown && f.HasGoal) {
			return
		}
	}
	s.handleCatalogued(PushChargingConnected, loadpointID, nil)
}
