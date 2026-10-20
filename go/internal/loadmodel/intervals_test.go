package loadmodel

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// householdStore records source ticks exactly as Core does, so the model
// trains on the same closed intervals as daily Home use.
func householdStore(t *testing.T) *state.Store {
	t.Helper()
	st := openTestDB(t)
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	return st
}

func recordHousehold(t *testing.T, st *state.Store, from, to time.Time, sources func(time.Time) state.HistorySources) {
	t.Helper()
	for at := from; !at.After(to); at = at.Add(5 * time.Second) {
		v := sources(at)
		v.AtMS, v.Topology = at.UnixMilli(), "site"
		if err := st.RecordTickWithOptionalHistory(&state.HistoryPoint{TsMs: at.UnixMilli(), JSON: `{}`, Sources: &v}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// trainingBase is a past, quarter-aligned hour; a first sample just before it
// places the cursor on it.
func trainingBase() time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
}

func TestTrainsOnceOnEachClosedValidHouseholdInterval(t *testing.T) {
	st := householdStore(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	base := trainingBase()
	s.sampleAt(base.Add(-time.Millisecond))
	q := TrainingInterval
	recordHousehold(t, st, base, base.Add(3*q), func(at time.Time) state.HistorySources {
		switch {
		case at.Before(base.Add(q)):
			return state.HistorySources{GridW: 1000}
		case at.Before(base.Add(2 * q)):
			// The battery is unknown for three minutes, longer than any gap
			// history may bridge as an estimate: this quarter is no label.
			v := state.HistorySources{GridW: 1000}
			if since := at.Sub(base); since >= q+5*time.Minute && since < q+8*time.Minute {
				v.Missing = state.MissingBattery
			}
			return v
		}
		// The battery supplies 200 W, so the house uses 800 W.
		return state.HistorySources{GridW: 600, BatteryW: -200}
	})
	s.sampleAt(base.Add(3*q + time.Minute))
	m := s.Model()
	if m.Samples != 2 {
		t.Fatalf("samples = %d, want the two valid quarters", m.Samples)
	}
	if got := m.Bucket[m.hourOfWeek(base)].Mean; got != 900 {
		t.Fatalf("hour mean = %v, want the mean of 1000 W and 800 W", got)
	}
	s.sampleAt(base.Add(3*q + 2*time.Minute))
	if s.Model().Samples != 2 {
		t.Fatal("a closed interval trained twice")
	}

	// The switch happens during the fourth quarter; it trains neither
	// profile, and the fifth trains the new one.
	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatal(err)
	}
	s.sampleAt(base.Add(3*q + 3*time.Minute))
	recordHousehold(t, st, base.Add(3*q+5*time.Second), base.Add(5*q), func(time.Time) state.HistorySources {
		return state.HistorySources{GridW: 500}
	})
	s.sampleAt(base.Add(5*q + time.Minute))
	snap := s.Snapshot()
	if snap.Profiles[ProfileHome].Samples != 2 || snap.Profiles[ProfileAway].Samples != 1 {
		t.Fatalf("profiles home=%d away=%d", snap.Profiles[ProfileHome].Samples, snap.Profiles[ProfileAway].Samples)
	}
}

func TestLearningCutoffSkipsIntervalsStartedBeforeIt(t *testing.T) {
	st := householdStore(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	base := trainingBase()
	s.sampleAt(base.Add(-time.Millisecond))
	if err := s.RestartLearning(base.Add(TrainingInterval / 2)); err != nil {
		t.Fatal(err)
	}
	// RestartLearning moves the cursor to the next interval after a sample.
	s.mu.Lock()
	s.nextIntervalMS = base.UnixMilli()
	s.mu.Unlock()
	recordHousehold(t, st, base, base.Add(2*TrainingInterval), func(time.Time) state.HistorySources {
		return state.HistorySources{GridW: 1000}
	})
	s.sampleAt(base.Add(2*TrainingInterval + time.Minute))
	if got := s.Model().Samples; got != 1 {
		t.Fatalf("samples = %d, want only the interval after the cutoff", got)
	}
}

// An interval read before a reconfiguration or learning restart must not
// train the new model, and the reset cursor never returns to it.
func TestIntervalReadInFlightIsDiscarded(t *testing.T) {
	for name, reset := range map[string]func(*Service) error{
		"reconfigure": func(s *Service) error {
			return s.Reconfigure("site", telemetry.ForecastOptions{}, "UTC", "b")
		},
		"restart learning": func(s *Service) error { return s.RestartLearning(time.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			st := householdStore(t)
			s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
			if err := s.Reconfigure("site", telemetry.ForecastOptions{}, "UTC", "a"); err != nil {
				t.Fatal(err)
			}
			base := trainingBase()
			s.sampleAt(base.Add(-time.Millisecond))
			recordHousehold(t, st, base, base.Add(TrainingInterval), func(time.Time) state.HistorySources {
				return state.HistorySources{GridW: 1000}
			})
			captured, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s.Temp = func(time.Time) (float64, bool) { close(captured); <-release; return 5, true }
			go func() { defer close(done); s.sampleAt(base.Add(TrainingInterval + time.Minute)) }()
			<-captured
			if err := reset(s); err != nil {
				t.Fatal(err)
			}
			close(release)
			<-done
			s.Temp = nil
			s.sampleAt(time.Now())
			if got := s.Model().Samples; got != 0 {
				t.Fatalf("interval from before the reset trained: %d samples", got)
			}
		})
	}
}
