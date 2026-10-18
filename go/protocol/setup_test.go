package protocol

import (
	"strings"
	"testing"
)

func TestSetupBoundaryRejectsExtraAuthorityAndOversizedPatches(t *testing.T) {
	for _, v := range []any{
		map[string]any{"json": []byte(`{}`), "path": "/api/agents"},
		SetupCall{JSON: []byte(`{"patch":"` + strings.Repeat("x", SetupRequestBytes) + `"}`)},
		SetupCall{JSON: []byte(`{"broken"`)},
	} {
		raw, _ := Marshal(v)
		if _, err := DecodeSetupCall(raw); err == nil {
			t.Fatal("invalid setup boundary accepted")
		}
	}
	raw, _ := Marshal(SetupCall{JSON: []byte(`{"patch":{"site":{"name":"Home"}}}`)})
	if _, err := DecodeSetupCall(raw); err != nil {
		t.Fatal(err)
	}
}
