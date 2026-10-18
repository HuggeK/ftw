package protocol

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestSetupCallGoldenVector(t *testing.T) {
	const vector = "a1646a736f6e4e7b227265766973696f6e223a317d"
	raw, err := Marshal(SetupCall{JSON: []byte(`{"revision":1}`)})
	if err != nil || hex.EncodeToString(raw) != vector {
		t.Fatal(err, hex.EncodeToString(raw))
	}
	golden, _ := hex.DecodeString(vector)
	call, err := DecodeSetupCall(golden)
	if err != nil || string(call.JSON) != `{"revision":1}` {
		t.Fatal(err, string(call.JSON))
	}
}

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
