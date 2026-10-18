package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestAgentInvitationVector(t *testing.T) {
	var v struct {
		Home, BoxHex, SecretHex, DeviceID, HashHex, SignedHex, Code string
		Request                                                     AgentInviteCreate
	}
	raw, err := os.ReadFile("testdata/invitation-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	box, _ := hex.DecodeString(v.BoxHex)
	secret, _ := hex.DecodeString(v.SecretHex)
	if got := hex.EncodeToString(AgentInviteHash(v.Home, secret)); got != v.HashHex {
		t.Fatalf("hash %s", got)
	}
	signed, err := InviteCreateBytes(SignedFor{BoxKey: box, DeviceID: v.DeviceID}, v.Request)
	if err != nil || hex.EncodeToString(signed) != v.SignedHex {
		t.Fatalf("signed bytes %x: %v", signed, err)
	}
	code, err := (AgentInviteCode{V: 1, Home: v.Home, Box: box, Secret: secret}).Encode()
	if err != nil || code != v.Code {
		t.Fatalf("code %s: %v", code, err)
	}
	decoded, err := ParseAgentInviteCode(code)
	if err != nil || !bytes.Equal(decoded.Secret, secret) {
		t.Fatal("code did not round trip")
	}
	for _, bad := range []string{"ABCD-EFGH", code + "=", code + "extra"} {
		if _, err := ParseAgentInviteCode(bad); err == nil {
			t.Fatalf("accepted malformed code %q", bad)
		}
	}
}
