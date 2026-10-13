package loadpoint

import (
	"time"

	"github.com/srcfl/ftw/go/internal/events"
)

const (
	// GoalRiskMinWh is the smallest shortfall worth telling anyone about.
	// Below it the plan is close enough that a rounding of the estimate or
	// a slightly cheaper slot changes the answer.
	GoalRiskMinWh = 1000
	// minCompletionWh keeps "car charged" for sessions that charged.
	minCompletionWh = 100
)

// ObservePlanShortfall takes a fresh plan's shortfall per loadpoint, in
// battery watt-hours, and raises charging.goal_at_risk once per plug-in for
// a car that will not reach its target by its ready time. Only when the box
// knows the car's level, from the car or from someone who set it: on an
// assumed level the shortfall is a guess, and the plug-in notification
// already asks for the level. The caller passes only plans that are neither
// being remade nor out of date.
func (m *Manager) ObservePlanShortfall(shortfallWh map[string]float64, now time.Time) {
	if m == nil || len(shortfallWh) == 0 {
		return
	}
	var fired []events.Event
	m.mu.Lock()
	bus := m.bus
	for _, id := range m.order {
		lp, ok := m.byID[id]
		if !ok || !lp.pluggedIn || lp.goalRiskNotified || !lp.socConfirmed || lp.completionNotified ||
			lp.targetSoC <= 0 || lp.targetTime.IsZero() || !lp.targetTime.After(now) {
			continue
		}
		wh := shortfallWh[id]
		if wh < GoalRiskMinWh {
			continue
		}
		lp.goalRiskNotified = true
		by := lp.targetTime.Local()
		if m.loc != nil {
			by = lp.targetTime.In(m.loc)
		}
		fired = append(fired, events.ChargingGoalAtRisk{
			LoadpointID: id, TargetSoC: lp.targetSoC, By: lp.targetTime, ByClock: by.Format("15:04"),
			ShortfallWh: wh, At: now,
		})
	}
	m.mu.Unlock()
	if bus == nil {
		return
	}
	for _, e := range fired {
		bus.Publish(e)
	}
}
