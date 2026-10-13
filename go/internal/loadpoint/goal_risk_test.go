package loadpoint

import (
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
)

func goalRiskLog(bus *events.Bus) func() []events.ChargingGoalAtRisk {
	var mu sync.Mutex
	var got []events.ChargingGoalAtRisk
	bus.Subscribe(events.KindChargingGoalAtRisk, func(e events.Event) {
		if ev, ok := e.(events.ChargingGoalAtRisk); ok {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	})
	return func() []events.ChargingGoalAtRisk {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.ChargingGoalAtRisk(nil), got...)
	}
}

// A car that reports nothing is done when the box's estimate, from a level
// someone set, reaches the target. Once, in kWh.
func TestACarWithoutItsOwnDataIsDoneAtTheBoxsEstimate(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.tick(0, true, 11_000, 0, true)
	r.mgr.SetCurrentSoC("garage", 0.5)
	r.tick(time.Hour, true, 11_000, 10_000, true)
	if c, _ := r.log.counts(); c != 0 {
		t.Fatal("done before the estimate reached the target")
	}
	need := 0.3 * 60_000 / DefaultChargeEfficiency
	r.tick(time.Hour, true, 11_000, need+50, true)
	if c, _ := r.log.counts(); c != 1 {
		t.Fatalf("complete events = %d, want 1", c)
	}
	if got := r.log.complete[0]; got.KWh < need/1000 || got.LoadpointID != "garage" {
		t.Fatalf("event %+v", got)
	}
	for range 5 {
		r.tick(10*time.Minute, true, 0, need+50, false)
	}
	if c, _ := r.log.counts(); c != 1 {
		t.Fatalf("complete events = %d after hours, want 1", c)
	}
}

// On an assumed level the estimate is a guess, so nothing claims the car is
// done.
func TestAnAssumedLevelNeverSaysDone(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.tick(0, true, 11_000, 0, true)
	r.tick(5*time.Hour, true, 11_000, 60_000, true)
	if c, _ := r.log.counts(); c != 0 {
		t.Fatalf("complete events = %d on an assumed level", c)
	}
}

// A car that reports its own level is done by its own reading, not by the
// box's estimate.
func TestACarWithItsOwnDataIsDoneOnlyByItsReading(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.tick(0, true, 11_000, 0, true)
	r.mgr.AnchorVehicleSoC("garage", 0.5)
	r.tick(5*time.Hour, true, 11_000, 30_000, true)
	if c, _ := r.log.counts(); c != 0 {
		t.Fatalf("complete events = %d from an estimate for a car that reports", c)
	}
}

// After a restart beside a car that is already done, nothing is said: it
// has not drawn power since the box started.
func TestARestartBesideAFinishedCarSaysNothing(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.tick(0, true, 0, 25_000, false)
	r.mgr.SetCurrentSoC("garage", 0.85)
	for range 5 {
		r.tick(time.Minute, true, 0, 25_000, false)
	}
	if c, _ := r.log.counts(); c != 0 {
		t.Fatalf("complete events = %d after a restart", c)
	}
}

// The plan's shortfall tells the household once per plug-in, and only when
// the level is known, the goal has a time ahead, and the gap matters.
func TestGoalAtRiskOncePerPlugInOnAKnownLevel(t *testing.T) {
	r := newSessionRig(t)
	got := goalRiskLog(r.mgr.bus)
	by := r.now.Add(8 * time.Hour)
	r.mgr.SetTarget("garage", 0.8, by)
	r.tick(0, true, 0, 0, true)

	short := map[string]float64{"garage": 9_000}
	r.mgr.ObservePlanShortfall(short, r.now)
	if len(got()) != 0 {
		t.Fatal("goal at risk on an assumed level")
	}
	r.mgr.SetCurrentSoC("garage", 0.2)
	r.mgr.ObservePlanShortfall(map[string]float64{"garage": GoalRiskMinWh - 1}, r.now)
	if len(got()) != 0 {
		t.Fatal("goal at risk for a gap under the threshold")
	}
	r.mgr.ObservePlanShortfall(short, r.now)
	r.mgr.ObservePlanShortfall(short, r.now.Add(time.Minute))
	evs := got()
	if len(evs) != 1 {
		t.Fatalf("goal at risk %d times, want once", len(evs))
	}
	if e := evs[0]; e.LoadpointID != "garage" || e.TargetSoC != 0.8 || !e.By.Equal(by) || e.ShortfallWh != 9_000 {
		t.Fatalf("event %+v", e)
	}

	// A new plug-in may speak again, but not about a ready time that passed.
	r.tick(time.Minute, false, 0, 0, false)
	r.tick(time.Minute, true, 0, 0, true)
	r.mgr.SetCurrentSoC("garage", 0.2)
	r.mgr.ObservePlanShortfall(short, by.Add(time.Minute))
	if len(got()) != 1 {
		t.Fatal("goal at risk for a ready time already gone")
	}
	r.mgr.ObservePlanShortfall(short, r.now)
	if len(got()) != 2 {
		t.Fatal("the next plug-in was not told")
	}
}
