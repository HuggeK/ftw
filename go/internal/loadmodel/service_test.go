package loadmodel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestResetPreservesHeatingCoefficient(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	s.SetHeatingCoef(275)

	s.Reset()

	if got := s.Model().HeatingW_per_degC; got != 275 {
		t.Fatalf("heating coefficient after reset = %v, want 275", got)
	}
}

// TestSeedHeatingCoefDoesNotOverwriteLearnedValue — restart on a deployment
// where the load model has been adapting must not clobber the learned
// HeatingW_per_degC with the operator-config seed. Without this guarantee
// every binary update silently throws away weeks of online fit. The
// startup wiring in cmd/ftw/main.go uses SeedHeatingCoef
// precisely because operator config is a *prior*, not an override.
func TestSeedHeatingCoefDoesNotOverwriteLearnedValue(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	// Simulate a model that has been adapting in production.
	s.mu.Lock()
	m := s.activeModelLocked()
	m.HeatingW_per_degC = 80.0
	m.Samples = 500
	s.mu.Unlock()

	s.SeedHeatingCoef(300) // operator config value carried into a restart

	if got := s.Model().HeatingW_per_degC; got != 80.0 {
		t.Errorf("learned heating coef must survive seed-only call: got %.0f, want 80", got)
	}
}

func TestSeedHeatingCoefAppliesOnColdStart(t *testing.T) {
	s := NewService(nil, telemetry.NewStore(), "site", 4000, 17250)
	// Fresh model — no samples observed yet.

	s.SeedHeatingCoef(300)

	if got := s.Model().HeatingW_per_degC; got != 300 {
		t.Errorf("cold-start seed should apply: got %.0f, want 300", got)
	}
}

func TestProfileAndModelsPersist(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	s := NewService(st, telemetry.NewStore(), "site", 4000, 17250)
	if err := s.SetProfile(ProfileAway); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	t0 := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	s.mu.Lock()
	for i := 0; i < 8; i++ {
		s.activeModelLocked().Update(t0.AddDate(0, 0, 7*i), 250, HeatingReferenceC)
	}
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	restored := NewService(st, telemetry.NewStore(), "site", 4000, 17250)
	if got := restored.Profile(); got != ProfileAway {
		t.Fatalf("restored profile = %q, want %q", got, ProfileAway)
	}
	if got := restored.Model().Samples; got != 8 {
		t.Fatalf("restored away samples = %d, want 8", got)
	}
}
