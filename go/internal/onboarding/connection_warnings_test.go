package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestSetupValidationPersistsConnectionWarningWithoutChangingSettings(t *testing.T) {
	live, err := config.Parse([]byte("fuse:\n  max_amps: 20\n  phases: 3\n  voltage: 230\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live.Revision = 1
	live.Loadpoints = []config.Loadpoint{{ID: "garage", DriverName: "removed-charger", MinChargeW: 1400, MaxChargeW: 7000}}
	store := draftStore(t)
	service := New(store)
	original, err := service.Edit(live, 0, json.RawMessage("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Checked(live.Revision, original.Revision, original.SHA256); !errors.Is(err, ErrConflict) {
		t.Fatal("unchecked charger warning was not bound to review")
	}
	checked, err := service.Validate(live, original.Revision, original.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.ValidationWarnings) != 1 || checked.Revision != original.Revision+1 {
		t.Fatal("warning-only validation was not saved for review")
	}
	if !bytes.Equal(checked.Config, original.Config) {
		t.Fatal("warning changed the saved charger choice")
	}
	resumed, err := New(store).Read(live)
	if err != nil || len(resumed.ValidationWarnings) != 1 || resumed.Revision != checked.Revision {
		t.Fatal("warning disappeared after reopening the draft")
	}
	again, err := service.Validate(live, checked.Revision, checked.SHA256)
	if err != nil || again.Revision != checked.Revision {
		t.Fatal("revalidation changed a reviewed warning")
	}
	if _, err := service.Checked(live.Revision, checked.Revision, checked.SHA256); err != nil {
		t.Fatal(err)
	}
	repaired, err := service.Edit(live, checked.Revision, json.RawMessage(`{"loadpoints":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	repaired, err = service.Validate(live, repaired.Revision, repaired.SHA256)
	if err != nil || len(repaired.ValidationWarnings) != 0 {
		t.Fatal("repair left an old warning")
	}
}
