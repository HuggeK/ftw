package config

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestLoadpointStepsAreLimitedOnLoadAndSave(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML+`
loadpoints:
  - id: Easee
    driver_name: easee_cloud
    min_charge_w: 4800
    max_charge_w: 11000
    allowed_steps_w: [0, 4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100, 12000]
`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11000}
	if !reflect.DeepEqual(cfg.Loadpoints[0].AllowedStepsW, want) {
		t.Fatal(cfg.Loadpoints[0].AllowedStepsW)
	}
	// This fixture also has no configured charger for its saved connection.
	if len(cfg.LoadpointWarnings) != 2 || !strings.Contains(cfg.LoadpointWarnings[0], `"Easee"`) {
		t.Fatal(cfg.LoadpointWarnings)
	}
	old := cfg.Loadpoints[0].AllowedStepsW
	cfg.Loadpoints[0].MaxChargeW = 10000
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if old[len(old)-1] != 11000 {
		t.Fatal("repair mutated an earlier snapshot")
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range reloaded.Loadpoints[0].AllowedStepsW {
		if step > 10000 {
			t.Fatalf("persisted step %v exceeds max", step)
		}
	}
	if len(reloaded.LoadpointWarnings) != 1 || !strings.HasPrefix(reloaded.LoadpointWarnings[0], missingChargerConnection) {
		t.Fatal("step correction should leave only the unresolved connection warning")
	}
}

func TestLoadpointStepsAreLimitedInStoredSettings(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConfigDatabase = database
	cfg.Loadpoints = []Loadpoint{{ID: "Easee", MaxChargeW: 11000, AllowedStepsW: []float64{0, 11100}}}
	if err := SaveStored(st, filepath.Join(dir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	doc, found, err := st.Configuration()
	if err != nil || !found {
		t.Fatalf("stored settings: found=%v err=%v", found, err)
	}
	// Inspect the stored bytes before load-time repair could hide a bad save.
	var saved storedSettings
	if err := json.Unmarshal(doc.Document, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Config.Loadpoints[0].AllowedStepsW[1] != 11000 || len(cfg.LoadpointWarnings) != 2 {
		t.Fatalf("stored steps=%v warnings=%v", saved.Config.Loadpoints[0].AllowedStepsW, cfg.LoadpointWarnings)
	}
}

func TestLoadpointStepsKeepInvalidPhysicsVisible(t *testing.T) {
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1)} {
		cfg, err := Parse([]byte(minimalYAML), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Loadpoints = []Loadpoint{{ID: "Easee", MaxChargeW: 11000, AllowedStepsW: []float64{invalid}}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid step %v", invalid)
		}
	}
}
