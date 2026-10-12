package protocol

import "testing"

// Field ids 1-9 are frozen permanently as of v1. An app of any age draws the
// core view from these alone, so a change here breaks installs nobody can
// reach.
func TestFrozenFieldIdsAreFrozen(t *testing.T) {
	want := map[Fid]string{
		1: "mode",
		2: "grid_w",
		3: "pv_w",
		4: "battery_w",
		5: "battery_soc",
		6: "load_w",
		7: "src_grid",
		8: "src_pv",
		9: "src_battery",
		// Extended deliberately on 2026-08-05 for EV charging. The freeze
		// covers the id's meaning, not its presence: like battery_soc,
		// ev_w is sent only when the hardware exists.
		10: "ev_w",
	}
	for fid, name := range want {
		if got := FrozenFieldNames[fid]; got != name {
			t.Fatalf("field %d is %q, and was frozen as %q", fid, got, name)
		}
	}
	if len(FrozenFieldNames) != len(want) {
		t.Fatalf("the frozen set has %d fields, want %d", len(FrozenFieldNames), len(want))
	}
}
