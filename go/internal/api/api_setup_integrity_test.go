package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func setupIntegrityCheck(d map[string]any) map[string]any {
	return map[string]any{"revision": d["revision"], "base_revision": d["base_revision"], "sha256": d["sha256"], "confirmed": true}
}

func TestSetupValidationReturnsTheNormalizedDraftBeforeConfirmation(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0,
		"patch": json.RawMessage(`{"planner":{"engine":"python","soc_min":0,"soc_min_pct":15}}`)})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	old := setupIntegrityCheck(draft)
	w, read := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || read["sha256"] != draft["sha256"] || !strings.Contains(w.Body.String(), `"soc_min_pct":15`) {
		t.Fatal("GET rewrote the draft or showed normalized data under an old hash", w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", old)
	if w.Code != http.StatusConflict {
		t.Fatal("unreviewed normalization saved", w.Code, w.Body.String())
	}
	w, normalized := setupCall(t, s, http.MethodPost, "/api/setup/validate", old)
	if w.Code != http.StatusOK || normalized["normalized"] != true || normalized["sha256"] == draft["sha256"] ||
		normalized["normalized_from_revision"] != draft["revision"] || !strings.Contains(w.Body.String(), `"soc_min":0.15`) {
		t.Fatal("normalization did not return a new reviewed candidate", w.Code, w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", old)
	if w.Code != http.StatusConflict {
		t.Fatal("old confirmation still valid", w.Code)
	}
	w, saved := setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(normalized))
	if w.Code != http.StatusOK || cfg.Planner.SoCMin != 0.15 || cfg.Planner.SoCMinPct != 0 || cfg.Planner.Engine != config.PlannerEngineEnergyplan {
		t.Fatal("saved settings differ from the checked draft", w.Code, w.Body.String())
	}
	if saved["commit"].(map[string]any)["sha256"] != normalized["sha256"] {
		t.Fatal("receipt bound the wrong configuration")
	}
	loaded, err := config.Load(s.deps.ConfigPath)
	if err != nil || loaded.Planner.SoCMin != cfg.Planner.SoCMin || loaded.Planner.Engine != cfg.Planner.Engine {
		t.Fatal("normalized save did not survive reload", err)
	}
}

func TestSetupUnrelatedEditsKeepExactVendorNumbersThroughSaveAndReload(t *testing.T) {
	s, cfg := setupTestServer(t)
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0,
		"patch": json.RawMessage(`{"drivers":[{"name":"meter","lua":"meter.lua","is_site_meter":true,"capabilities":{"standalone":true},"config":{"vendor_id":9007199254740993,"unsigned_id":18446744073709551615,"nested":[{"unit_id":1,"ratio":0.1}],"string_id":"9007199254740993"}}]}`)})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"],
		"patch": map[string]any{"site": map[string]any{"name": "Unrelated change"}}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"vendor_id":9007199254740993`) ||
		!strings.Contains(w.Body.String(), `"unsigned_id":18446744073709551615`) {
		t.Fatal("draft/status rounded an untouched identifier", w.Body.String())
	}
	w, checked := setupCall(t, s, http.MethodPost, "/api/setup/validate", setupIntegrityCheck(draft))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(checked))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	loaded, err := config.Load(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*config.Config{cfg, loaded} {
		c := snapshot.Drivers[0].Config
		if fmt.Sprint(c["vendor_id"]) != "9007199254740993" || fmt.Sprint(c["unsigned_id"]) != "18446744073709551615" || c["string_id"] != "9007199254740993" {
			t.Fatal("Settings save or reload changed an identifier", c)
		}
	}
	// The recovery export keeps integer scalars, rather than quoting a new
	// special numeric type or rounding through a JSON map.
	doc, found, err := s.deps.State.Configuration()
	if err != nil || !found {
		t.Fatal("missing stored configuration", err)
	}
	path := filepath.Join(t.TempDir(), "recovery.yaml")
	if err := config.ExportStored(path, doc, loaded.ConfigDatabase); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "vendor_id: 9007199254740993") || !strings.Contains(string(raw), "unsigned_id: 18446744073709551615") {
		t.Fatal("recovery export changed numeric identifiers", string(raw), err)
	}
}

func setupIntegrityLiveDriver(t *testing.T, s *Server, cfg *config.Config) {
	t.Helper()
	cfg.Drivers = []config.Driver{{Name: "meter", Lua: filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers", "old.lua"),
		IsSiteMeter: true, Capabilities: config.Capabilities{Standalone: true},
		Config: map[string]any{"password": "old-password", "refresh_token": "old-token"}}}
	if err := config.SaveStored(s.deps.State, s.deps.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestSetupChangingDriverImplementationDoesNotRestoreOldSecretsAtSave(t *testing.T) {
	s, cfg := setupTestServer(t)
	setupIntegrityLiveDriver(t, s, cfg)
	w, draft := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	drivers := draft["config"].(map[string]any)["drivers"].([]any)
	driver := drivers[0].(map[string]any)
	driver["lua"] = "drivers/new.lua"
	driver["config"] = map[string]any{"password": "", "refresh_token": ""}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/validate", setupIntegrityCheck(draft))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(draft))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	loaded, err := config.Load(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*config.Config{cfg, loaded} {
		if c.Drivers[0].Config["password"] != "" || c.Drivers[0].Config["refresh_token"] != "" {
			t.Fatal("old driver secrets crossed into the reviewed replacement")
		}
	}
}

func TestSetupCannotRebindAReplacementTokenToTheLiveOwnerAfterReview(t *testing.T) {
	s, cfg := setupTestServer(t)
	setupIntegrityLiveDriver(t, s, cfg)
	liveOwner := cfg.Drivers[0].CredentialOwner
	w, draft := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	drivers := draft["config"].(map[string]any)["drivers"].([]any)
	driver := drivers[0].(map[string]any)
	driver["config"].(map[string]any)["refresh_token"] = "new-token"
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	bound := draft
	drivers = draft["config"].(map[string]any)["drivers"].([]any)
	owner := drivers[0].(map[string]any)["credential_owner"]
	drivers[0].(map[string]any)["credential_owner"] = liveOwner
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatal("caller could replace a bound credential owner", w.Code, w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || draft["sha256"] != bound["sha256"] {
		t.Fatal("refused owner edit changed draft", w.Body.String())
	}
	// Omitting the technical owner field keeps the current named identity,
	// including a new draft-only token and the other masked credentials.
	drivers = draft["config"].(map[string]any)["drivers"].([]any)
	delete(drivers[0].(map[string]any), "credential_owner")
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": draft["revision"], "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, draft = setupCall(t, s, http.MethodPost, "/api/setup/validate", setupIntegrityCheck(draft))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(draft))
	if w.Code != http.StatusOK || cfg.Drivers[0].CredentialOwner != owner || cfg.Drivers[0].Config["refresh_token"] != "new-token" || cfg.Drivers[0].Config["password"] != "old-password" {
		t.Fatal("save changed the reviewed owner", w.Code, w.Body.String())
	}
	loaded, err := config.Load(s.deps.ConfigPath)
	if err != nil || loaded.Drivers[0].CredentialOwner != owner || loaded.Drivers[0].Config["refresh_token"] != "new-token" || loaded.Drivers[0].Config["password"] != "old-password" {
		t.Fatal("credential binding or secrets changed after reload", err)
	}
}

func TestSetupFinalSaveRefusesLateMutationBeforeAnySQLWrite(t *testing.T) {
	s, cfg := setupTestServer(t)
	beforeRevision := cfg.Revision
	w, draft := setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"site": map[string]any{"name": "Reviewed"}}})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	s.deps.SaveConfigWithValues = func(path string, candidate *config.Config, values map[string]string, reviewedJSON []byte, beforeSave func() error) error {
		candidate.Site.Name = "Not reviewed"
		return config.SaveStoredReviewedWithCheck(s.deps.State, path, candidate, values, reviewedJSON, beforeSave)
	}
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(draft))
	if w.Code != http.StatusConflict || cfg.Revision != beforeRevision || cfg.Site.Name != "Before" {
		t.Fatal("late mutation reached live settings", w.Code, w.Body.String())
	}
	loaded, err := config.Load(s.deps.ConfigPath)
	if err != nil || loaded.Revision != beforeRevision || loaded.Site.Name != "Before" {
		t.Fatal("late mutation reached SQL", err)
	}
	if receipt, err := s.setup.Receipt(); err != nil || receipt != nil {
		t.Fatal("refused save left a success receipt", err)
	}
}

func TestSetupEquivalentDriverPathsKeepSecretsAndCanBeCommitted(t *testing.T) {
	for _, spelling := range []string{"./drivers/old.lua", "absolute"} {
		t.Run(spelling, func(t *testing.T) {
			s, cfg := setupTestServer(t)
			setupIntegrityLiveDriver(t, s, cfg)
			w, draft := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			drivers := draft["config"].(map[string]any)["drivers"].([]any)
			path := spelling
			if spelling == "absolute" {
				path = cfg.Drivers[0].Lua
			}
			drivers[0].(map[string]any)["lua"] = path
			w, draft = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": drivers}})
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			w, draft = setupCall(t, s, http.MethodPost, "/api/setup/validate", setupIntegrityCheck(draft))
			if w.Code != http.StatusOK || draft["normalization_required"] != false {
				t.Fatal("path normalization cannot complete", w.Body.String())
			}
			w, _ = setupCall(t, s, http.MethodPost, "/api/setup/commit", setupIntegrityCheck(draft))
			if w.Code != http.StatusOK || cfg.Drivers[0].Config["password"] != "old-password" || cfg.Drivers[0].Config["refresh_token"] != "old-token" {
				t.Fatal("equivalent path lost a credential or could not save", w.Code, w.Body.String())
			}
		})
	}
}

func TestSetupChangingDriverRefusesAnUnresolvedMaskedToken(t *testing.T) {
	s, cfg := setupTestServer(t)
	setupIntegrityLiveDriver(t, s, cfg)
	w, before := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	drivers := before["config"].(map[string]any)["drivers"].([]any)
	drivers[0].(map[string]any)["lua"] = "drivers/replacement.lua"
	w, _ = setupCall(t, s, http.MethodPost, "/api/setup/draft", map[string]any{"revision": 0, "patch": map[string]any{"drivers": drivers}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatal("a different driver accepted the earlier driver's masked token", w.Code, w.Body.String())
	}
	w, after := setupCall(t, s, http.MethodGet, "/api/setup/draft", nil)
	if w.Code != http.StatusOK || after["sha256"] != before["sha256"] || after["revision"] != before["revision"] {
		t.Fatal("refused secret edit replaced the draft", w.Body.String())
	}
}
