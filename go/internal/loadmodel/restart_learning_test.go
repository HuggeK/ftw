package loadmodel

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestRestartLearningClearsEveryLoadProfileAndRestoresConfiguredPrior(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 0)
	if err := s.Reconfigure("site", telemetry.ForecastOptions{}, "Europe/Stockholm", "site-a"); err != nil {
		t.Fatal(err)
	}
	s.SeedHeatingCoef(275)
	for _, profile := range Profiles() {
		m := s.models[profile]
		m.Update(time.Now(), 2000, 5)
		m.HeatingW_per_degC = 900
	}
	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}

	snap := s.Snapshot()
	if snap.ActiveProfile != ProfileAway {
		t.Fatalf("active profile changed to %q", snap.ActiveProfile)
	}
	for _, profile := range Profiles() {
		got := snap.Profiles[profile]
		if got.Samples != 0 || got.MAE != 0 || got.LastMs != 0 || got.HasTemperature {
			t.Fatalf("learned %s state survived: %+v", profile, got)
		}
		if got.HeatingW_per_degC != 275 {
			t.Fatalf("%s heating prior = %.0f, want configured 275", profile, got.HeatingW_per_degC)
		}
		if got.PeakW != 4000 || got.Timezone != "Europe/Stockholm" || got.ConfigRevision != "site-a" {
			t.Fatalf("configured %s state changed: %+v", profile, got)
		}
		if got.LearningStartedMS != cutoff.UnixMilli() {
			t.Fatalf("%s cutoff = %d", profile, got.LearningStartedMS)
		}
	}
}

func TestRestartLearningLoadRetryPersistsAllProfilesWithoutErasingNewSamples(t *testing.T) {
	bad := openTestDB(t)
	s := NewService(bad, telemetry.NewStore(), "site", 4000, 0)
	s.SeedHeatingCoef(275)
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	if err := s.RestartLearning(cutoff); err == nil {
		t.Fatal("closed store did not surface persistence failure")
	}
	for _, profile := range Profiles() {
		if !s.models[profile].Update(cutoff.Add(time.Minute), 1000, 10) {
			t.Fatalf("post-reset %s sample rejected", profile)
		}
	}

	good, err := state.Open(t.TempDir() + "/retry.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { good.Close() })
	s.Store = good
	if err := s.RestartLearning(cutoff); err != nil {
		t.Fatal(err)
	}
	restored := NewService(good, telemetry.NewStore(), "site", 4000, 0)
	if restored.LearningStartedMS() != cutoff.UnixMilli() {
		t.Fatalf("restored cutoff = %d", restored.LearningStartedMS())
	}
	for _, profile := range Profiles() {
		if got := restored.models[profile]; got.Samples != 1 || got.LearningStartedMS != cutoff.UnixMilli() {
			t.Fatalf("same-cutoff retry lost %s learning: %+v", profile, got)
		}
	}
}

func TestLoadRestoreReconcilesPartiallyPersistedEpoch(t *testing.T) {
	st := openTestDB(t)
	s := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	at := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	for _, profile := range Profiles() {
		s.models[profile].Update(at, 1000, 10)
	}
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}

	cutoff := at.Add(time.Hour).UnixMilli()
	home := freshProfile(s.models[ProfileHome], ProfileHome, "UTC", cutoff, nil)
	js, err := modelstate.Wrap(FeatureHash(), home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(stateKey(ProfileHome), js); err != nil {
		t.Fatal(err)
	}

	restored := NewService(st, telemetry.NewStore(), "site", 4000, 0)
	for _, profile := range Profiles() {
		got := restored.models[profile]
		if got.Samples != 0 || got.LearningStartedMS != cutoff {
			t.Fatalf("partial epoch left stale %s state: %+v", profile, got)
		}
	}
}
