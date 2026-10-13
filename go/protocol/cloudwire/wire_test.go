package cloudwire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
)

// testKey derives a fixed P-256 key from a label, so vectors and tests do not
// depend on a random draw.
func testKey(t testing.TB, label string) *ecdsa.PrivateKey {
	t.Helper()
	for i := byte(0); ; i++ {
		d := sha256.Sum256(append([]byte(label), i))
		key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d[:])
		if err == nil {
			return key
		}
	}
}

func homeOf(t testing.TB, key *ecdsa.PrivateKey) (string, []byte) {
	t.Helper()
	pk, err := PublicKeyBytesOf(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return HomeID(pk), pk
}

func TestFrameIsHeaderAndEnvelopeAndNothingElse(t *testing.T) {
	env := []byte{0xa3, 1, 2, 3}
	frame, err := EncodeFrame(env)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 1, 0, 0, 0, 4, 0xa3, 1, 2, 3}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = % x, want % x", frame, want)
	}
	got, err := DecodeFrame(frame)
	if err != nil || !bytes.Equal(got, env) {
		t.Fatalf("decode = % x, %v", got, err)
	}
}

func TestDecodeFrameRefusesWhatThisProtocolNeverSends(t *testing.T) {
	good := []byte{1, 1, 0, 0, 0, 2, 0xa0, 0xa0}
	cases := map[string][]byte{
		"short":        {1, 1, 0},
		"version":      {2, 1, 0, 0, 0, 2, 0xa0, 0xa0},
		"lane 0":       {1, 0, 0, 0, 0, 2, 0xa0, 0xa0},
		"flag":         {1, 1, 2, 0, 0, 2, 0xa0, 0xa0},
		"reserved":     {1, 1, 0, 1, 0, 2, 0xa0, 0xa0},
		"padding":      append(append([]byte{}, good...), 0),
		"overrun":      {1, 1, 0, 0, 0, 9, 0xa0},
		"short length": {1, 1, 0, 0, 0, 1, 0xa0, 0xa0},
	}
	for name, msg := range cases {
		if _, err := DecodeFrame(msg); !errors.Is(err, ErrFrame) {
			t.Errorf("%s: err = %v, want ErrFrame", name, err)
		}
	}
	if _, err := DecodeFrame(good); err != nil {
		t.Fatalf("good frame refused: %v", err)
	}
}

func TestEnvelopeKeepsAnEmptySignatureAsBytes(t *testing.T) {
	raw, err := Envelope{T: MsgWatch, B: []byte{0xa1, 0x62, 'h', 'z', 1}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// {"b": h'a16268 7a01', "t": "watch", "sig": h''}
	want := []byte{0xa3, 0x61, 'b', 0x45, 0xa1, 0x62, 'h', 'z', 1, 0x61, 't', 0x65, 'w', 'a', 't', 'c', 'h', 0x63, 's', 'i', 'g', 0x40}
	if !bytes.Equal(raw, want) {
		t.Fatalf("envelope = % x\nwant       % x", raw, want)
	}
	back, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.T != MsgWatch || len(back.Sig) != 0 || !bytes.Equal(back.B, want[4:9]) {
		t.Fatalf("decoded %+v", back)
	}
}

func TestDecodeEnvelopeRefusesTheWrongShapes(t *testing.T) {
	mk := func(v any) []byte {
		b, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := map[string][]byte{
		"not a map":    mk([]int{1}),
		"no type":      mk(map[string]any{"b": []byte{1}, "sig": []byte{}}),
		"no body":      mk(map[string]any{"t": "hb", "sig": []byte{}}),
		"no sig":       mk(map[string]any{"t": "hb", "b": []byte{1}}),
		"text body":    mk(map[string]any{"t": "hb", "b": "a0", "sig": []byte{}}),
		"bytes type":   mk(map[string]any{"t": []byte("hb"), "b": []byte{1}, "sig": []byte{}}),
		"empty type":   mk(map[string]any{"t": "", "b": []byte{1}, "sig": []byte{}}),
		"text sig":     mk(map[string]any{"t": "hb", "b": []byte{1}, "sig": ""}),
		"garbage":      {0xff, 0x00},
		"duplicate t":  {0xa4, 0x61, 'b', 0x41, 0, 0x61, 't', 0x61, 'x', 0x61, 't', 0x61, 'y', 0x63, 's', 'i', 'g', 0x40},
		"truncated":    {0xa3, 0x61, 'b'},
		"integer body": mk(map[string]any{"t": "hb", "b": 5, "sig": []byte{}}),
	}
	for name, raw := range cases {
		if _, err := DecodeEnvelope(raw); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestSignatureCoversHomeTypeAndEveryBodyByte(t *testing.T) {
	key := testKey(t, "box")
	home, _ := homeOf(t, key)
	env, err := Seal(key, home, MsgHB, HB{Header: Header{Seq: 7, Boot: make([]byte, BootBytes)}, Clock: ClockNTP})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(&key.PublicKey, home, env); err != nil {
		t.Fatalf("own signature: %v", err)
	}

	for i := range env.B {
		changed := env
		changed.B = append([]byte(nil), env.B...)
		changed.B[i] ^= 0x01
		if err := Verify(&key.PublicKey, home, changed); !errors.Is(err, ErrSignature) {
			t.Fatalf("byte %d flipped and the signature still verified", i)
		}
	}
	other := env
	other.T = MsgLive
	if Verify(&key.PublicKey, home, other) == nil {
		t.Fatal("the signature does not cover the type")
	}
	if Verify(&key.PublicKey, "aaaaaaaaaaaaaaaaaaaaaaaaaa", env) == nil {
		t.Fatal("the signature does not cover the home")
	}
	if Verify(&testKey(t, "someone else").PublicKey, home, env) == nil {
		t.Fatal("another key verified the box's signature")
	}
	short := env
	short.Sig = env.Sig[:63]
	if Verify(&key.PublicKey, home, short) == nil {
		t.Fatal("a short signature verified")
	}
}

func TestUnsignedSealLeavesTheSignatureEmpty(t *testing.T) {
	env, err := Seal(nil, "home", MsgWatch, Watch{Hz: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Sig) != 0 {
		t.Fatalf("sig = % x", env.Sig)
	}
}

func TestHomeIDIsTwentySixLowercaseCharacters(t *testing.T) {
	_, pk := homeOf(t, testKey(t, "box"))
	home := HomeID(pk)
	if len(home) != 26 {
		t.Fatalf("home %q has %d characters", home, len(home))
	}
	for _, c := range home {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			t.Fatalf("home %q has %q", home, c)
		}
	}
	if HomeID(pk) != home {
		t.Fatal("home id is not a function of the key")
	}
}

func TestOpaqueIDsHideTheNameAndChangeWithTheGrant(t *testing.T) {
	a := bytes.Repeat([]byte{1}, GrantKeyBytes)
	b := bytes.Repeat([]byte{2}, GrantKeyBytes)
	id := SourceID(a, "sungrow")
	if len(id) != 11 || bytes.Contains([]byte(id), []byte("sungrow")) {
		t.Fatalf("source id %q", id)
	}
	if SourceID(a, "sungrow") != id {
		t.Fatal("source id is not stable within a grant")
	}
	if SourceID(b, "sungrow") == id {
		t.Fatal("a new grant kept the same source id")
	}
	if LoadpointID(a, "sungrow") == id {
		t.Fatal("a loadpoint and a source with one name share an id")
	}
}

func TestPublicKeyRoundTrips(t *testing.T) {
	key := testKey(t, "box")
	pk, err := PublicKeyBytesOf(&key.PublicKey)
	if err != nil || len(pk) != PublicKeyBytes {
		t.Fatalf("pk %d bytes, %v", len(pk), err)
	}
	back, err := ParsePublicKey(pk)
	if err != nil || !back.Equal(&key.PublicKey) {
		t.Fatalf("parse: %v", err)
	}
	if _, err := ParsePublicKey(pk[:64]); err == nil {
		t.Fatal("a 64-byte key parsed")
	}
	bad := append([]byte(nil), pk...)
	bad[64] ^= 1
	if _, err := ParsePublicKey(bad); err == nil {
		t.Fatal("a point off the curve parsed")
	}
}

func TestHeaderIsFlattenedIntoEveryBody(t *testing.T) {
	wall := int64(1_700_000_000_000)
	raw, err := Marshal(HB{Header: Header{Seq: 3, Boot: []byte{9}, UptimeMs: 10, WallMs: &wall}, Clock: ClockNTP})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "boot", "uptimeMs", "wallMs", "clock", "srcRev", "liveSeq", "lpSeq", "ages"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("hb has no %q: %v", k, fields)
		}
	}
	if _, ok := fields["Header"]; ok {
		t.Error("the header is nested, not flattened")
	}
	// Without a synced clock, wallMs is absent rather than zero.
	raw, _ = Marshal(HB{Header: Header{Seq: 3, Boot: []byte{9}}})
	fields = nil
	_ = Unmarshal(raw, &fields)
	if _, ok := fields["wallMs"]; ok {
		t.Error("wallMs sent without a clock")
	}
}

func TestNilListsAreEmptyAndNilNumbersAreNull(t *testing.T) {
	raw, err := Marshal(Feedback{Kind: "battery"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = Unmarshal(raw, &fields)
	if v, ok := fields["actualW"]; !ok || v != nil {
		t.Errorf("actualW = %v (present %v), want null", v, ok)
	}
	raw, _ = Marshal(Live{})
	fields = nil
	_ = Unmarshal(raw, &fields)
	if v, ok := fields["dispatchBlockedBy"].([]any); !ok || len(v) != 0 {
		t.Errorf("dispatchBlockedBy = %#v, want []", fields["dispatchBlockedBy"])
	}
	if _, ok := fields["sources"]; ok {
		t.Error("an empty source table was sent")
	}
}

func TestSignUsesTheRandomSource(t *testing.T) {
	key := testKey(t, "box")
	a, _ := Sign(rand.Reader, key, "h", MsgHB, []byte{1})
	b, _ := Sign(rand.Reader, key, "h", MsgHB, []byte{1})
	if bytes.Equal(a, b) {
		t.Fatal("two ECDSA signatures were identical")
	}
}

func FuzzDecodeFrameAndEnvelope(f *testing.F) {
	f.Add([]byte{1, 1, 0, 0, 0, 2, 0xa0, 0xa0})
	env, _ := Envelope{T: MsgHB, B: []byte{0xa0}}.Encode()
	frame, _ := EncodeFrame(env)
	f.Add(frame)
	f.Fuzz(func(t *testing.T, msg []byte) {
		payload, err := DecodeFrame(msg)
		if err != nil {
			return
		}
		e, err := DecodeEnvelope(payload)
		if err != nil {
			return
		}
		// Whatever decodes re-encodes to something that decodes the same.
		again, err := e.Encode()
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		back, err := DecodeEnvelope(again)
		if err != nil || back.T != e.T || !bytes.Equal(back.B, e.B) || !bytes.Equal(back.Sig, e.Sig) {
			t.Fatalf("round trip changed the envelope: %v", err)
		}
	})
}
