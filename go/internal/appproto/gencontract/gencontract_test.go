package gencontract

import (
	"strings"
	"testing"
)

// A delta and FTW Cloud's live body key fields by id alone, so one id or one
// name in both blocks is two meanings for one number. An extra field among
// the frozen ids would be dropped after the core view when lane 0 runs out of
// room, which is the wrong way round.
func TestGenerateRefusesFieldsThatCollide(t *testing.T) {
	const frozen = "version: 1\nfrozen_fields:\n  1: { name: mode, kind: enum, unit: null }\n  10: { name: ev_w, kind: power, unit: W }\n"
	cases := map[string]string{
		"same id":         "extra_fields:\n  10: { name: grid_l1_da, kind: current, unit: dA }\n",
		"among frozen":    "extra_fields:\n  5: { name: grid_l1_da, kind: current, unit: dA }\n",
		"same name":       "extra_fields:\n  11: { name: ev_w, kind: power, unit: W }\n",
		"two extra names": "extra_fields:\n  11: { name: x, kind: current, unit: dA }\n  12: { name: x, kind: current, unit: dA }\n",
	}
	for name, extra := range cases {
		if _, err := Generate([]byte(frozen + extra)); err == nil {
			t.Errorf("%s: generated without complaint", name)
		}
	}

	out, err := Generate([]byte(frozen + "extra_fields:\n  11: { name: grid_l1_da, kind: current, unit: dA }\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FidGridL1Da Fid = 11", `FidGridL1Da: "grid_l1_da"`, `FidGridL1Da: "dA"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("generated file lacks %s", want)
		}
	}
	// The frozen table stays the frozen set alone.
	frozenTable := string(out)[strings.Index(string(out), "var FrozenFieldNames"):strings.Index(string(out), "var FrozenFieldUnits")]
	if strings.Contains(frozenTable, "grid_l1_da") {
		t.Fatal("an extra field landed in the frozen table")
	}
}
