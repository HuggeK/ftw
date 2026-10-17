package main

import (
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

// A reset makes now the mark even when now is lower, and the mark it keeps
// is the one a restart reads.
func TestTheClockMarkResetsLower(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := &wallMark{highMs: 4_000_000_000_000}
	w.saved = w.highMs
	if err := w.reset(st, 1_800_000_000_000); err != nil {
		t.Fatal(err)
	}
	again := &wallMark{}
	again.load(st)
	if again.highMs != 1_800_000_000_000 || w.highMs != 1_800_000_000_000 {
		t.Fatalf("mark %d, after a restart %d", w.highMs, again.highMs)
	}
}

// The box page's browser looks a code up on the host this build links to,
// so a local test build with its own FTW Cloud finds its own lookup.
func TestTheCodeLookupFollowsTheUplink(t *testing.T) {
	for endpoint, want := range map[string]string{
		"wss://cloud.ftw.energy/v1/box": "https://cloud.ftw.energy/oauth/requests",
		"wss://127.0.0.1:18943/v1/box":  "https://127.0.0.1:18943/oauth/requests",
		"ws://127.0.0.1:18944/v1/box":   "http://127.0.0.1:18944/oauth/requests",
		"":                              "",
		"not a url with a host":         "",
	} {
		if got := codeLookup(endpoint); got != want {
			t.Errorf("%q: %q, want %q", endpoint, got, want)
		}
	}
}
