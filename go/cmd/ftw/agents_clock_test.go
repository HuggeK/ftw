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
