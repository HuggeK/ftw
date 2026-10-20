package loadmodel

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestRestoredMinuteHeatingBoundsKeepLearning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coldMinutes int64
		wantHeating float64
		wantDays    int
	}{
		{name: "thirty minutes cannot close a cold day", coldMinutes: 30, wantHeating: 500, wantDays: 2},
		{name: "old minutes and a new quarter make six hours", coldMinutes: 345, wantHeating: 300, wantDays: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestDB(t)
			day := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
			stored := make(map[Profile]Model)
			for _, profile := range Profiles() {
				m := *trainedModel(t)
				m.Timezone = "UTC"
				m.ConfigRevision = "existing-revision"
				m.LearningStartedMS = day.AddDate(0, 0, -30).UnixMilli()
				m.Samples = 1200
				m.LastMs = day.Add(6 * time.Hour).UnixMilli()
				m.PriorScale = newProfileModel(m.PeakW, profile).PriorScale
				m.HeatingW_per_degC = 500
				// State written by the old minute sampler, including a
				// partial cold day and two qualified earlier days.
				m.HeatingBoundDay = day.Format("2006-01-02")
				m.HeatingBoundSamples = tc.coldMinutes
				m.HeatingBoundLoadSumW = float64(tc.coldMinutes) * 1000
				m.HeatingBoundDeltaSumC = float64(tc.coldMinutes) * 10
				m.HeatingBoundRecent = [heatingBoundDays]float64{0, 100, 100}
				m.HeatingBoundRecentN = 2
				stored[profile] = m
				js, err := modelstate.Wrap(FeatureHash(), m)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.SaveConfig(stateKey(profile), js); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SaveConfig(profileStateKey, string(ProfileAway)); err != nil {
				t.Fatal(err)
			}

			s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
			snapshot := s.Snapshot()
			if snapshot.ActiveProfile != ProfileAway {
				t.Fatal("restore changed the active profile")
			}
			for _, profile := range Profiles() {
				if snapshot.Profiles[profile] != stored[profile] {
					t.Fatalf("restore changed learned state or partial-day evidence for %s", profile)
				}
			}

			quarterMidpoint := day.Add(6*time.Hour + TrainingInterval/2)
			s.mu.Lock()
			trained := true
			for _, profile := range Profiles() {
				m := s.models[profile]
				if !m.Update(quarterMidpoint, 1000, 8) {
					trained = false
				}
			}
			s.mu.Unlock()
			if !trained {
				t.Fatal("new quarter did not train")
			}
			for profile, m := range s.Snapshot().Profiles {
				if m.HeatingBoundSamples != tc.coldMinutes+15 ||
					m.HeatingBoundLoadSumW != float64(tc.coldMinutes+15)*1000 ||
					m.HeatingBoundDeltaSumC != float64(tc.coldMinutes+15)*10 {
					t.Fatalf("%s mixed minute and quarter units: %d minutes, %g load sum, %g temperature sum",
						profile, m.HeatingBoundSamples, m.HeatingBoundLoadSumW, m.HeatingBoundDeltaSumC)
				}
			}
			if err := s.persist(); err != nil {
				t.Fatal(err)
			}
			restarted := NewService(st, telemetry.NewStore(), "site", 4000, 0)
			for profile, m := range restarted.Snapshot().Profiles {
				if m != s.Snapshot().Profiles[profile] {
					t.Fatalf("restart changed migrated state for %s", profile)
				}
				m.Update(day.AddDate(0, 0, 1), 1000, HeatingReferenceC)
				if m.HeatingW_per_degC != tc.wantHeating || m.HeatingBoundRecentN != tc.wantDays {
					t.Fatalf("%s closed %d old minutes plus a quarter as %d cold days at %g W/C; want %d days at %g W/C",
						profile, tc.coldMinutes, m.HeatingBoundRecentN, m.HeatingW_per_degC, tc.wantDays, tc.wantHeating)
				}
			}
		})
	}
}
