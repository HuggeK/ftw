package appwire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// The vector's initiator ephemeral, from the same Cacophony entry. The
// responder test does not need it; the initiator cannot run without it.
const cacophonyInitEphemeral = "893e28b9dc6ca8d611ab664754b8ceb7bac5117349a4439a6b0569da977c464a"

func TestCacophonyVectorAsInitiator(t *testing.T) {
	eph := keyFromHex(t, cacophonyInitEphemeral)
	initiator, err := NewInitiator(InitiatorOptions{
		StaticKey:    keyFromHex(t, cacophony.initStatic),
		RemoteStatic: unhex(t, cacophony.initRemoteStatc),
		Prologue:     unhex(t, cacophony.prologue),
		Ephemeral:    &eph,
	})
	if err != nil {
		t.Fatalf("NewInitiator: %v", err)
	}
	message1, err := initiator.WriteMessage(unhex(t, cacophony.messages[0].payload))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got := hex.EncodeToString(message1); got != cacophony.messages[0].ciphertext {
		t.Fatalf("message 1 = %s, want %s", got, cacophony.messages[0].ciphertext)
	}
	payload, err := initiator.ReadMessage(unhex(t, cacophony.messages[1].ciphertext))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := hex.EncodeToString(payload); got != cacophony.messages[1].payload {
		t.Fatalf("message 2 payload = %s, want %s", got, cacophony.messages[1].payload)
	}
	session, err := initiator.Split()
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if got := hex.EncodeToString(session.HandshakeHash); got != cacophony.handshakeHash {
		t.Fatalf("handshake hash = %s, want %s", got, cacophony.handshakeHash)
	}
	for i, msg := range cacophony.messages[2:] {
		if i%2 == 0 {
			ciphertext, err := session.Send.EncryptWithAD(nil, unhex(t, msg.payload))
			if err != nil || hex.EncodeToString(ciphertext) != msg.ciphertext {
				t.Fatalf("transport %d: %x %v", i, ciphertext, err)
			}
			continue
		}
		plaintext, err := session.Recv.DecryptWithAD(nil, unhex(t, msg.ciphertext))
		if err != nil || hex.EncodeToString(plaintext) != msg.payload {
			t.Fatalf("transport %d: %x %v", i, plaintext, err)
		}
	}
	if _, err := initiator.Split(); !errors.Is(err, ErrNoiseState) {
		t.Fatalf("second Split: %v", err)
	}
}

// An initiator and the responder agree with each other, and a transport
// built on each side carries frames both ways.
func TestInitiatorAndResponderMeet(t *testing.T) {
	phone, _ := GenerateKeyPair()
	service, _ := GenerateKeyPair()
	prologue := []byte("ftw.cloud.v1:test")
	init, err := NewInitiator(InitiatorOptions{StaticKey: phone, RemoteStatic: service.Public(), Prologue: prologue})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewResponder(HandshakeOptions{StaticKey: service, Prologue: prologue})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RemoteStatic() != nil {
		t.Fatal("the responder named a key before message 1")
	}
	m1, _ := init.WriteMessage([]byte("now"))
	p1, err := resp.ReadMessage(m1)
	if err != nil || string(p1) != "now" {
		t.Fatalf("message 1: %q %v", p1, err)
	}
	if !bytes.Equal(resp.RemoteStatic(), phone.Public()) {
		t.Fatal("after message 1 the responder does not know the initiator's key")
	}
	m2, _ := resp.WriteMessage([]byte("snapshot"))
	p2, err := init.ReadMessage(m2)
	if err != nil || string(p2) != "snapshot" {
		t.Fatalf("message 2: %q %v", p2, err)
	}
	is, _ := init.Split()
	rs, _ := resp.Split()
	if !bytes.Equal(rs.RemoteStatic, phone.Public()) || !bytes.Equal(is.HandshakeHash, rs.HandshakeHash) {
		t.Fatal("the two ends disagree about the session")
	}
	a, b := NewTransport(is), NewTransport(rs)
	msg, _ := b.Encrypt([]byte("frame"))
	got, err := a.Decrypt(msg)
	if err != nil || string(got) != "frame" {
		t.Fatalf("transport: %q %v", got, err)
	}
}

func TestInitiatorRefusesAnotherResponder(t *testing.T) {
	phone, _ := GenerateKeyPair()
	service, _ := GenerateKeyPair()
	impostor, _ := GenerateKeyPair()
	init, _ := NewInitiator(InitiatorOptions{StaticKey: phone, RemoteStatic: service.Public()})
	resp, _ := NewResponder(HandshakeOptions{StaticKey: impostor})
	m1, _ := init.WriteMessage(nil)
	if _, err := resp.ReadMessage(m1); !errors.Is(err, ErrNoiseAuth) {
		t.Fatalf("an impostor read message 1: %v", err)
	}
	if _, err := NewInitiator(InitiatorOptions{StaticKey: phone, RemoteStatic: []byte{1, 2}}); !errors.Is(err, ErrNoiseKey) {
		t.Fatalf("short responder key: %v", err)
	}
}
