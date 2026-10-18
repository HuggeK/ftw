package onboarding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestEntryEditPreservesALargeListAndItsCredentials(t *testing.T) {
	live := testLive(t)
	live.Drivers = []config.Driver{
		{Name: "Meter", Lua: "drivers/meter.lua", IsSiteMeter: true, Config: map[string]any{"password": strings.Repeat("a", 20<<10)}},
		{Name: "Battery", Lua: "drivers/battery.lua", Config: map[string]any{"password": "keep-battery-secret"}},
	}
	s := New(draftStore(t))
	d, err := s.EditOperations(live, 0, json.RawMessage(`[{"op":"replace","path":"/drivers/1/name","value":"Garage battery"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(d.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Drivers) != 2 || cfg.Drivers[0].Config["password"] != live.Drivers[0].Config["password"] || cfg.Drivers[1].Name != "Garage battery" || cfg.Drivers[1].Config["password"] != "keep-battery-secret" {
		t.Fatal("entry edit removed a sibling or credential")
	}
	if _, err := s.EditOperations(live, 0, json.RawMessage(`[{"op":"remove","path":"/drivers/0"}]`)); err != ErrConflict {
		t.Fatal("stale list edit did not conflict", err)
	}
}

func TestEntryEditsRejectInvalidPointersAndApplyAllOrNothing(t *testing.T) {
	live := testLive(t)
	s := New(draftStore(t))
	for _, ops := range []string{
		`[{"op":"replace","path":"/missing","value":1}]`,
		`[{"op":"remove","path":"/drivers/99"}]`,
		`[{"op":"replace","path":"/site/name","value":"Changed"},{"op":"move","path":"/site/id"}]`,
		`[{"op":"replace","path":"/site/name","value":"Changed"},{"op":"replace","path":"/site/no_such_field","value":1}]`,
		`[{"op":"replace","path":"/site/~5","value":1}]`,
	} {
		if _, err := s.EditOperations(live, 0, json.RawMessage(ops)); err == nil {
			t.Fatal("invalid edit accepted", ops)
		}
		d, err := s.Read(live)
		if err != nil || d.Revision != 0 {
			t.Fatal("failed operation changed the draft", err)
		}
	}
	d, err := s.EditOperations(live, 0, json.RawMessage(`[{"op":"replace","path":"/drivers","value":[]},{"op":"add","path":"/drivers/-","value":{"name":"New meter","lua":"drivers/new.lua","config":{"host":"192.168.1.8"}}},{"op":"remove","path":"/drivers/0"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != 1 {
		t.Fatal(d.Revision)
	}
}
