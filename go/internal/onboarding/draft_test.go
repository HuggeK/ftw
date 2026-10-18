package onboarding

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func testLive(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("site:\n  name: Original\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\nprice:\n  provider: entsoe\n  api_key: saved-secret\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = 7
	return c
}

func draftStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDraftResumesFromDurableStorageWithoutChangingLiveConfig(t *testing.T) {
	live := testLive(t)
	store := draftStore(t)
	s := New(store)
	first, err := s.Read(live)
	if err != nil || first.Revision != 0 {
		t.Fatalf("initial: %+v %v", first, err)
	}
	if _, exists := store.LoadConfig(draftKey); exists {
		t.Fatal("reading wrote a draft")
	}
	draft, err := s.Edit(live, 0, json.RawMessage(`{"site":{"name":"My home"},"price":{"zone":"SE3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// A new service has no in-memory draft to restore from.
	resumed, err := New(store).Read(live)
	if err != nil || resumed.SHA256 != draft.SHA256 || resumed.Revision != 1 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	checked, err := New(store).Checked(live.Revision, resumed.Revision, resumed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Site.Name != "My home" || checked.Price.APIKey != "saved-secret" || checked.Price.Zone != "SE3" {
		t.Fatal("patch lost an unedited value or credential")
	}
	if live.Site.Name != "Original" || live.Price.Zone != "" {
		t.Fatal("draft changed live config")
	}
}

func TestDraftConflictsProtectBothDraftAndLiveSettings(t *testing.T) {
	live := testLive(t)
	s := New(draftStore(t))
	draft, err := s.Edit(live, 0, json.RawMessage(`{"site":{"name":"First"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Edit(live, 0, json.RawMessage(`{"site":{"name":"Lost update"}}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.Checked(live.Revision, draft.Revision, "another digest"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	live.Revision++
	if _, err = s.Edit(live, draft.Revision, json.RawMessage(`{"site":{"name":"Old base"}}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.Checked(live.Revision, draft.Revision, draft.SHA256); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.Restart(live, 0); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	restarted, err := s.Restart(live, draft.Revision)
	if err != nil || restarted.BaseRevision != live.Revision || restarted.Revision != 2 {
		t.Fatalf("restart: %+v %v", restarted, err)
	}
	if strings.Contains(string(restarted.Config), "First") {
		t.Fatal("restart kept the stale draft")
	}
}

func TestIncompleteDraftCanResumeButCannotPassValidation(t *testing.T) {
	live := testLive(t)
	s := New(draftStore(t))
	draft, err := s.Edit(live, 0, json.RawMessage(`{"fuse":{"max_amps":0},"drivers":[{"name":"new meter"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Checked(live.Revision, draft.Revision, draft.SHA256); err == nil {
		t.Fatal("unfinished driver passed validation")
	}
	resumed, err := s.Read(live)
	if err != nil || resumed.SHA256 != draft.SHA256 {
		t.Fatal("validation changed the saved draft", err)
	}
}

func TestMalformedOrUnknownFieldsNeverReplaceTheDraft(t *testing.T) {
	live := testLive(t)
	s := New(draftStore(t))
	for _, raw := range []string{
		`null`, `[]`, `{} {}`, `{"unknown":1}`, `{"site":{"not_a_field":1}}`,
		`{"fuse":{"max_amps":"sixteen"}}`, `{"site":{"name":"A","name":"B"}}`,
		`{"drivers":[{"name":"A","name":"B"}]}`, `{"site":{`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := s.Edit(live, 0, json.RawMessage(raw)); err == nil {
				t.Fatal("accepted invalid patch")
			}
			if got, err := s.Read(live); err != nil || got.Revision != 0 {
				t.Fatal("invalid patch changed the draft", err)
			}
		})
	}
}

type failedStore struct{ Storage }

func (s failedStore) SaveConfig(string, string) error { return errors.New("disk full") }

func TestFailedDraftSaveLeavesPreviousRevisionIntact(t *testing.T) {
	live := testLive(t)
	store := draftStore(t)
	draft, err := New(store).Edit(live, 0, json.RawMessage(`{"site":{"name":"Saved"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(failedStore{store}).Edit(live, draft.Revision, json.RawMessage(`{"site":{"name":"Unsaved"}}`)); err == nil {
		t.Fatal("reported a failed save as success")
	}
	got, err := New(store).Read(live)
	if err != nil || got.SHA256 != draft.SHA256 {
		t.Fatal("failed save replaced saved state", err)
	}
}

func TestConcurrentEditsHaveOneWinner(t *testing.T) {
	live := testLive(t)
	s := New(draftStore(t))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"First", "Second"} {
		wg.Go(func() {
			_, err := s.Edit(live, 0, json.RawMessage(`{"site":{"name":"`+name+`"}}`))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("%d winners, %d conflicts", winners, conflicts)
	}
}

func TestDamagedDraftIsNotSilentlyReplaced(t *testing.T) {
	live := testLive(t)
	store := draftStore(t)
	if err := store.SaveConfig(draftKey, `{"version":99}`); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	if _, err := s.Read(live); err == nil {
		t.Fatal("read hid a damaged draft")
	}
	if _, err := s.Edit(live, 0, json.RawMessage(`{}`)); err == nil {
		t.Fatal("edit replaced a damaged draft")
	}
}

func TestUnavailableStorageDoesNotLookLikeANewInstallation(t *testing.T) {
	live := testLive(t)
	store := draftStore(t)
	s := New(store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(live); err == nil {
		t.Fatal("closed storage looked like an empty draft")
	}
	if _, err := s.Receipt(); err == nil {
		t.Fatal("closed storage looked like an uncommitted draft")
	}
}

func TestSchemaCoversAllPublicConfigFieldsWithoutRuntimeAuthority(t *testing.T) {
	live := testLive(t)
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	schema := Schema()
	props := schema["properties"].(map[string]any)
	for field := range fields {
		if _, ok := props[field]; !ok {
			t.Fatalf("schema omitted public field %q", field)
		}
	}
	for _, field := range []string{"Revision", "revision", "LANPasswordHash", "lan_password_hash", "config_database"} {
		if _, ok := props[field]; ok {
			t.Fatalf("schema exposed runtime field %q", field)
		}
	}
	for _, field := range []string{"site", "fuse", "drivers", "price", "weather", "batteries", "loadpoints", "vehicles", "ocpp", "homeassistant", "notifications"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("schema omitted setup section %q", field)
		}
	}
}
