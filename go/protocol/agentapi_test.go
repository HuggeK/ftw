package protocol

import (
	"encoding/hex"
	"testing"
)

func TestDiagnosticAPIReadVector(t *testing.T) {
	// CBOR map(1), text(4) path, text(19) /api/energy/history.
	body, err := hex.DecodeString("a16470617468732f6170692f656e657267792f686973746f7279")
	if err != nil {
		t.Fatal(err)
	}
	read, err := DecodeAPIRead(body)
	if err != nil || read.Path != "/api/energy/history" || read.MaxBytes != 0 {
		t.Fatalf("read vector: %+v %v", read, err)
	}
	// A method field cannot add a write path to this read.
	bad, _ := hex.DecodeString("a26470617468732f6170692f656e657267792f686973746f7279666d6574686f6464504f5354")
	if _, err := DecodeAPIRead(bad); err == nil {
		t.Fatal("unknown method field accepted")
	}
}

func TestDiagnosticPageVector(t *testing.T) {
	body, err := hex.DecodeString("a46470617468732f6170692f656e657267792f686973746f7279666f6666736574187b686d617842797465731a000100006d617263686976654d656d626572612e")
	if err != nil {
		t.Fatal(err)
	}
	read, err := DecodeAPIRead(body)
	if err != nil || read.Offset != 123 || read.MaxBytes != 65536 || read.ArchiveMember != "." {
		t.Fatalf("page vector: %+v %v", read, err)
	}
}
func TestDiagnosticPathSyntaxDoesNotKeepAnAPIList(t *testing.T) {
	for _, value := range []string{"/api/future/read", "/api/drivers/charger-192.168.1.9/source", "/api/drivers/EV%20charger%2Fgarage/logs", "/api/drivers/battery%25name"} {
		if !ValidAgentAPIPath(value) {
			t.Errorf("valid path refused: %s", value)
		}
	}
	for _, value := range []string{"/status", "/api/../config", "/api/status?x=1", "/api/status#x", "/api/%2e%2e/config", "/api/a%0Ab", "/api/a%ZZ", "/api/a\\b", "/api/a\n"} {
		if ValidAgentAPIPath(value) {
			t.Errorf("invalid path accepted: %q", value)
		}
	}
}
