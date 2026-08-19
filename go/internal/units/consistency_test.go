package units_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/forecast"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/units"
)

// These tests are the harness: if a later change puts kWp or 0–100 SoC
// back into core structs, they fail. Doors (optimizer JSON, appproto)
// are allowed their own types.

func TestPVArrayCoreFieldIsRatedWatts(t *testing.T) {
	typ := reflect.TypeOf(config.PVArray{})
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("config.PVArray must store RatedW (watts)")
	}
	f, ok := typ.FieldByName("KWp")
	if ok {
		tag := f.Tag.Get("yaml")
		if tag != "-" && tag != "kwp,omitempty" {
			t.Fatalf("legacy KWp must be omitempty yaml only, got %q", tag)
		}
	}
}

func TestForecastArrayHasNoKWp(t *testing.T) {
	typ := reflect.TypeOf(forecast.Array{})
	if _, ok := typ.FieldByName("KWp"); ok {
		t.Fatal("forecast.Array must not have KWp; store RatedW and convert at the forecast.solar URL")
	}
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("forecast.Array must store RatedW (watts)")
	}
}

func TestMPCParamsSoCIsFraction(t *testing.T) {
	typ := reflect.TypeOf(mpc.Params{})
	for _, banned := range []string{"SoCMinPct", "SoCMaxPct", "InitialSoCPct"} {
		if _, ok := typ.FieldByName(banned); ok {
			t.Fatalf("mpc.Params.%s must not exist; use 0–1 SoCMin/SoCMax/InitialSoC", banned)
		}
	}
	for _, want := range []string{"SoCMin", "SoCMax", "InitialSoC"} {
		if _, ok := typ.FieldByName(want); !ok {
			t.Fatalf("mpc.Params.%s missing", want)
		}
	}
}

func TestActionJSONSoCIsFractionNotPercent(t *testing.T) {
	a := mpc.Action{SoC: 0.55, LoadpointSoC: 0.80}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["soc_pct"]; ok {
		t.Fatalf("Action JSON still emits soc_pct: %s", raw)
	}
	if _, ok := m["loadpoint_soc_pct"]; ok {
		t.Fatalf("Action JSON still emits loadpoint_soc_pct: %s", raw)
	}
	soc, ok := m["soc"].(float64)
	if !ok || soc != 0.55 {
		t.Fatalf("soc = %v (%T), want 0.55; json=%s", m["soc"], m["soc"], raw)
	}
	if !units.ValidFraction(soc) {
		t.Fatalf("soc %v is not a 0–1 fraction", soc)
	}
}

func TestPlanJSONSoCIsFraction(t *testing.T) {
	p := mpc.Plan{InitialSoC: 0.42, Actions: []mpc.Action{{SoC: 0.5}}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["initial_soc_pct"]; ok {
		t.Fatalf("Plan JSON still emits initial_soc_pct: %s", raw)
	}
	got, ok := m["initial_soc"].(float64)
	if !ok || got != 0.42 {
		t.Fatalf("plan initial_soc = %v, want 0.42; json=%s", m["initial_soc"], raw)
	}
}

func TestNameplateSumsRatedWatts(t *testing.T) {
	arrays := []forecast.Array{
		{TiltDeg: 27, AzimuthDeg: 150, RatedW: 12960},
		{TiltDeg: 27, AzimuthDeg: 240, RatedW: 6000},
	}
	if got := forecast.NameplateW(18960, arrays); got != 18960 {
		t.Fatalf("nameplate = %v, want 18960 W", got)
	}
}
