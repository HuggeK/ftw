package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestValidationBindsNormalizedConfigurationToANewReviewedDraft(t *testing.T) {
	for _, tc := range []struct {
		name, patch, expected string
		warning               bool
	}{
		{"legacy units", `{"planner":{"soc_min":0,"soc_min_pct":15,"engine":"python"}}`, `"soc_min":0.15`, false},
		{"legacy email", `{"ev_charger":{"provider":"easee","email":"test@example.com"}}`, `"username":"test@example.com"`, false},
		{"charger steps", `{"loadpoints":[{"id":"Easee","driver_name":"easee_cloud","min_charge_w":1400,"max_charge_w":7000,"allowed_steps_w":[0,1400,11000]}]}`, `"allowed_steps_w":[0,1400,7000]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := testLive(t)
			store := draftStore(t)
			s := New(store)
			original, err := s.Edit(live, 0, json.RawMessage(tc.patch))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Checked(live.Revision, original.Revision, original.SHA256); !errors.Is(err, ErrConflict) {
				t.Fatal("commit accepted unreviewed normalization", err)
			}
			read, err := s.Read(live)
			if err != nil || read.SHA256 != original.SHA256 || !bytes.Equal(read.Config, original.Config) {
				t.Fatal("read or refused commit changed draft", err)
			}
			checked, err := s.Validate(live, original.Revision, original.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			if checked.Revision != original.Revision+1 || checked.SHA256 == original.SHA256 ||
				checked.NormalizedFromRevision != original.Revision || !bytes.Contains(checked.Config, []byte(tc.expected)) {
				t.Fatalf("normalization did not produce a reviewable draft: %+v", checked)
			}
			if tc.warning && len(checked.ValidationWarnings) == 0 {
				t.Fatal("normalization warning was lost")
			}
			// A lost validation reply resumes the same normalized bytes and warning.
			resumed, err := New(store).Read(live)
			if err != nil || resumed.SHA256 != checked.SHA256 || (tc.warning && len(resumed.ValidationWarnings) == 0) {
				t.Fatal("normalized receipt did not resume", err)
			}
			if _, err := s.Checked(live.Revision, original.Revision, original.SHA256); !errors.Is(err, ErrConflict) {
				t.Fatal("old confirmation survived normalization", err)
			}
			candidate, err := s.Checked(live.Revision, checked.Revision, checked.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(candidate)
			if err != nil || !bytes.Equal(raw, checked.Config) {
				t.Fatal("candidate differs from reviewed bytes", err)
			}
			again, err := s.Validate(live, checked.Revision, checked.SHA256)
			if err != nil || again.Revision != checked.Revision || again.SHA256 != checked.SHA256 {
				t.Fatal("validating twice changed the receipt", err)
			}
		})
	}
}

func TestFailedOrInvalidNormalizationKeepsTheOldDraft(t *testing.T) {
	live := testLive(t)
	store := draftStore(t)
	s := New(store)
	d, err := s.Edit(live, 0, json.RawMessage(`{"planner":{"soc_min_pct":15},"drivers":[{"name":"unfinished"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(live, d.Revision, d.SHA256); err == nil {
		t.Fatal("incomplete setup passed")
	}
	after, err := s.Read(live)
	if err != nil || after.SHA256 != d.SHA256 || !bytes.Equal(after.Config, d.Config) {
		t.Fatal("invalid validation wrote half-normalized data", err)
	}
	// A persistence failure also leaves the original revision retryable.
	d, err = s.Edit(live, d.Revision, json.RawMessage(`{"drivers":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	broken := New(failedStore{store})
	if _, err := broken.Validate(live, d.Revision, d.SHA256); err == nil {
		t.Fatal("normalization save failure hidden")
	}
	after, err = s.Read(live)
	if err != nil || after.SHA256 != d.SHA256 || !bytes.Equal(after.Config, d.Config) {
		t.Fatal("failed save changed draft", err)
	}
}

func TestValidationRepairsAnOlderDraftCredentialBindingBeforeReview(t *testing.T) {
	live := testLive(t)
	live.Drivers = []config.Driver{{Name: "meter", Lua: "meter.lua", IsSiteMeter: true, CredentialOwner: "live-owner",
		Capabilities: config.Capabilities{Standalone: true}, Config: map[string]any{"refresh_token": "old"}}}
	store := draftStore(t)
	s := New(store)
	d, err := s.Edit(live, 0, json.RawMessage(`{"site":{"name":"Changed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a draft from an earlier version, with a new token still bound
	// to the live token family. Validation must change the reviewed version.
	var cfg config.Config
	if err := config.DecodeJSON(d.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Drivers[0].Config["refresh_token"] = "new"
	d.Config, err = json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.SHA256 = digest(d.Config, d.BaseRevision)
	raw, _ := json.Marshal(d)
	if err := store.SaveConfig(draftKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	next, err := s.Validate(live, d.Revision, d.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if next.SHA256 == d.SHA256 || next.Revision != d.Revision+1 {
		t.Fatal("late credential binding kept the old confirmation")
	}
	if err := config.DecodeJSON(next.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Drivers[0].CredentialOwner == "live-owner" || cfg.Drivers[0].CredentialOwner == "" {
		t.Fatal("replacement token family was not bound")
	}
	again, err := s.Validate(live, next.Revision, next.SHA256)
	if err != nil || again.SHA256 != next.SHA256 {
		t.Fatal("credential binding changed on retry", err)
	}
}
