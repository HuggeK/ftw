package cloudwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/srcfl/ftw/go/protocol/appwire"
)

// testdata/cloud-app-vectors.json freezes one app session byte for byte:
// fixed static and ephemeral keys on both ends, message 1 with now, message 2
// with a snapshot of the signed vectors in cloud-vectors.json, and transport
// messages both ways. srcfl/ftw-cloud answers it as the service; the phone
// replays it as the initiator.

const appVectorsPath = "testdata/cloud-app-vectors.json"

type appVectorFile struct {
	ServiceStatic    string            `json:"serviceStatic"`
	ServicePublic    string            `json:"servicePublic"`
	ServiceEphemeral string            `json:"serviceEphemeral"`
	PhoneStatic      string            `json:"phoneStatic"`
	PhonePublic      string            `json:"phonePublic"`
	PhoneEphemeral   string            `json:"phoneEphemeral"`
	Home             string            `json:"home"`
	Prologue         string            `json:"prologue"`
	Msg1Payload      string            `json:"msg1Payload"`
	Msg1             string            `json:"msg1"`
	Msg2Payload      string            `json:"msg2Payload"`
	Msg2             string            `json:"msg2"`
	HandshakeHash    string            `json:"handshakeHash"`
	Snapshot         []string          `json:"snapshot"`
	Transport        []appVectorFrame  `json:"transport"`
	Pairing          appVectorPairing  `json:"pairing"`
	Notes            map[string]string `json:"notes"`
}

// appVectorPairing is a new phone's proof: the code it scanned, the hash the
// roster lists and now.pair carries, and the device id the service names it
// by until the roster lists it. Msg1NowTun is the same now with the box
// session's message 1 riding along, as a phone sends it to save a round trip.
type appVectorPairing struct {
	Code       string `json:"code"`
	Hash       string `json:"hash"`
	Msg1Now    string `json:"msg1Now"`
	Msg1NowTun string `json:"msg1NowTun"`
	DeviceID   string `json:"deviceId"`
}

type appVectorFrame struct {
	From    string `json:"from"`
	Name    string `json:"name"`
	Frame   string `json:"frame"`
	Message string `json:"message"`
}

func fixedX25519(t *testing.T, label string) appwire.KeyPair {
	t.Helper()
	sum := sha256.Sum256([]byte(label))
	k, err := appwire.KeyPairFromSecret(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func secretHex(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func TestAppSessionVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var signed vectorFile
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	envelope := func(name string) []byte {
		for _, m := range signed.Messages {
			if m.Name == name {
				b, _ := hex.DecodeString(m.Envelope)
				return b
			}
		}
		t.Fatalf("no signed vector %q", name)
		return nil
	}
	frameOf := func(env []byte) []byte {
		f, err := EncodeFrame(env)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	labels := map[string]string{
		"serviceStatic":    "ftw.cloud app vector service static",
		"serviceEphemeral": "ftw.cloud app vector service ephemeral",
		"phoneStatic":      "ftw.cloud app vector phone static",
		"phoneEphemeral":   "ftw.cloud app vector phone ephemeral",
	}
	serviceStatic := fixedX25519(t, labels["serviceStatic"])
	serviceEph := fixedX25519(t, labels["serviceEphemeral"])
	phoneStatic := fixedX25519(t, labels["phoneStatic"])
	phoneEph := fixedX25519(t, labels["phoneEphemeral"])
	home := signed.BoxKey.Home
	prologue := AppPrologue(home)

	msg1Payload, err := Unsigned(MsgNow, Now{Hz: HzLooking})
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot: newest live with sources, newest live, lp, prices and hb,
	// in rising seq.
	names := []string{"live full", "live delta", "lp", "prices", "hb"}
	var envs [][]byte
	for _, n := range names {
		envs = append(envs, envelope(n))
	}
	msg2Payload, err := SnapshotPayload(envs, 1_791_820_869_000, true, 0)
	if err != nil {
		t.Fatal(err)
	}

	init, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: phoneStatic, RemoteStatic: serviceStatic.Public(), Prologue: prologue, Ephemeral: &phoneEph,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := appwire.NewResponder(appwire.HandshakeOptions{StaticKey: serviceStatic, Prologue: prologue, Ephemeral: &serviceEph})
	if err != nil {
		t.Fatal(err)
	}
	msg1, err := init.WriteMessage(msg1Payload)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := resp.ReadMessage(msg1)
	if err != nil || !bytes.Equal(got1, msg1Payload) {
		t.Fatalf("message 1: %v", err)
	}
	msg2, err := resp.WriteMessage(msg2Payload)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := init.ReadMessage(msg2)
	if err != nil || !bytes.Equal(got2, msg2Payload) {
		t.Fatalf("message 2: %v", err)
	}
	snap, err := ParseSnapshot(got2)
	if err != nil || len(snap.Envelopes) != len(envs) || !snap.Online {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	for i := range envs {
		if !bytes.Equal(snap.Envelopes[i], envs[i]) {
			t.Fatalf("snapshot entry %d is not the box's bytes", i)
		}
	}
	is, _ := init.Split()
	rs, _ := resp.Split()
	phone, service := appwire.NewTransport(is), appwire.NewTransport(rs)

	out := appVectorFile{
		ServiceStatic: secretHex(labels["serviceStatic"]), ServicePublic: hex.EncodeToString(serviceStatic.Public()),
		ServiceEphemeral: secretHex(labels["serviceEphemeral"]),
		PhoneStatic:      secretHex(labels["phoneStatic"]), PhonePublic: hex.EncodeToString(phoneStatic.Public()),
		PhoneEphemeral: secretHex(labels["phoneEphemeral"]),
		Home:           home, Prologue: hex.EncodeToString(prologue),
		Msg1Payload: hex.EncodeToString(msg1Payload), Msg1: hex.EncodeToString(msg1),
		Msg2Payload: hex.EncodeToString(msg2Payload), Msg2: hex.EncodeToString(msg2),
		HandshakeHash: hex.EncodeToString(rs.HandshakeHash),
		Notes: map[string]string{
			"keys":      "Secrets are SHA-256 of the labels in app_vectors_test.go. X25519 clamps them.",
			"snapshot":  "Message 2 holds the cloud-vectors.json envelopes named here, in this order.",
			"transport": "Each message is appwire's: u64 BE seq, then ChaCha20-Poly1305 over the frame with the seq as associated data. Each direction counts from 0.",
		},
	}
	out.Snapshot = names

	addFrame := func(from, name string, frame []byte) {
		var msg []byte
		var err error
		if from == "service" {
			msg, err = service.Encrypt(frame)
			if err == nil {
				var back []byte
				if back, err = phone.Decrypt(msg); err == nil && !bytes.Equal(back, frame) {
					t.Fatalf("%s did not round-trip", name)
				}
			}
		} else {
			msg, err = phone.Encrypt(frame)
			if err == nil {
				var back []byte
				if back, err = service.Decrypt(msg); err == nil && !bytes.Equal(back, frame) {
					t.Fatalf("%s did not round-trip", name)
				}
			}
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out.Transport = append(out.Transport, appVectorFrame{From: from, Name: name, Frame: hex.EncodeToString(frame), Message: hex.EncodeToString(msg)})
	}
	addFrame("service", "hb", frameOf(envelope("hb")))
	addFrame("service", "lp", frameOf(envelope("lp")))
	away, _ := Unsigned(MsgWatch, Watch{Hz: HzAway})
	addFrame("phone", "watch away", frameOf(away))
	suspend, _ := Unsigned(MsgSuspend, Suspend{DeviceID: "Zf09xQ2a"})
	addFrame("phone", "suspend", frameOf(suspend))
	// The phone's own session with its box rides as tun: d is opaque here.
	tunUp, _ := Unsigned(MsgTun, AppTun{Sid: 7, D: bytes.Repeat([]byte{0x9a}, 48)})
	addFrame("phone", "tun", frameOf(tunUp))
	tunDown, _ := Unsigned(MsgTun, AppTun{Sid: 7, D: bytes.Repeat([]byte{0x99}, 112)})
	addFrame("service", "tun", frameOf(tunDown))
	tunEnd, _ := Unsigned(MsgTun, AppTun{Sid: 7, D: []byte{}})
	addFrame("service", "tun end", frameOf(tunEnd))

	code := bytes.Repeat([]byte{0x88}, 16)
	pairNow, _ := Unsigned(MsgNow, Now{Hz: HzLooking, Pair: PairingHash(home, code)})
	pairNowTun, _ := Unsigned(MsgNow, Now{Hz: HzLooking, Pair: PairingHash(home, code),
		Tun: &AppTun{Sid: 1, D: bytes.Repeat([]byte{0x9b}, 112)}})
	out.Pairing = appVectorPairing{
		Code: hex.EncodeToString(code), Hash: hex.EncodeToString(PairingHash(home, code)),
		Msg1Now: hex.EncodeToString(pairNow), Msg1NowTun: hex.EncodeToString(pairNowTun),
		DeviceID: DeviceID(phoneStatic.Public()),
	}

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(appVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(appVectorsPath)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the bytes no longer match %s. If the wire change is intended, "+
			"regenerate with -update and pair the change with srcfl/ftw-cloud and the phone", appVectorsPath)
	}
}

func TestAppPrologueAndHome(t *testing.T) {
	if got := string(AppPrologue("fjzyr67buzpyncqmjliwgezvde")); got != "ftw.cloud.v1:fjzyr67buzpyncqmjliwgezvde" {
		t.Fatalf("prologue %q", got)
	}
	for s, ok := range map[string]bool{
		"fjzyr67buzpyncqmjliwgezvde":  true,
		"fjzyr67buzpyncqmjliwgezvd":   false,
		"fjzyr67buzpyncqmjliwgezvdee": false,
		"Fjzyr67buzpyncqmjliwgezvde":  false,
		"fjzyr67buzpyncqmjliwgezvd1":  false,
		"fjzyr67buzpyncqmjliwgezvd/":  false,
	} {
		if ValidHome(s) != ok {
			t.Fatalf("ValidHome(%q) = %v", s, !ok)
		}
	}
}

func TestSnapshotRefusesWhatIsNotAnEnvelope(t *testing.T) {
	if _, err := SnapshotPayload([][]byte{{0xa0}}, 0, false, 0); err == nil {
		t.Fatal("a snapshot took an empty map as an envelope")
	}
	now, _ := Unsigned(MsgNow, Now{Hz: 1})
	if _, err := ParseSnapshot(now); err == nil {
		t.Fatal("a now envelope read as a snapshot")
	}
}
