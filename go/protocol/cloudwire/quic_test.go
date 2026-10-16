package cloudwire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// testdata/cloud-quic-vectors.json freezes the QUIC framing: the ALPN names,
// varints at each width, message prefixes at the lengths where they change,
// the two targets, and which of the uplink's envelopes may go as a datagram.
// srcfl/ftw-cloud and the phone replay it.
//
//	go test ./cloudwire -run TestQUICVectors -update

const quicVectorsPath = "testdata/cloud-quic-vectors.json"

type quicVectorFile struct {
	ALPN       map[string]string    `json:"alpn"`
	Transports []string             `json:"transports"`
	Varints    []quicVectorVarint   `json:"varints"`
	Messages   []quicVectorMessage  `json:"messages"`
	Targets    []quicVectorTarget   `json:"targets"`
	Datagrams  []quicVectorDatagram `json:"datagrams"`
	Notes      map[string]string    `json:"notes"`
}

type quicVectorVarint struct {
	Value uint64 `json:"value"`
	Hex   string `json:"hex"`
}

type quicVectorMessage struct {
	Len int `json:"len"`
	// Prefix is the varint the message starts with.
	Prefix string `json:"prefix"`
}

type quicVectorTarget struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	// Message is the target as it goes first on the control stream.
	Message string `json:"message"`
}

type quicVectorDatagram struct {
	// Name is the envelope's name in cloud-vectors.json.
	Name     string `json:"name"`
	Envelope string `json:"envelope"`
	Datagram bool   `json:"datagram"`
}

func TestQUICVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var signed vectorFile
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}

	want := quicVectorFile{
		ALPN:       map[string]string{"box": ALPNBox, "app": ALPNApp},
		Transports: Transports,
		Notes: map[string]string{
			"message":  "one WebSocket binary message: its length as a QUIC varint (RFC 9000 §16), then its bytes",
			"target":   "the first message on the control stream (client bidirectional stream 0), ASCII",
			"datagram": "hb, and live without the sources key, may go as one QUIC DATAGRAM; nothing else",
		},
	}
	// RFC 9000 §A.1's own examples, then each width's edges.
	for _, v := range []uint64{0, 37, 63, 64, 15293, 16383, 16384, 494878333, 1<<30 - 1, 1 << 30, 151288809941952652, maxVarint} {
		want.Varints = append(want.Varints, quicVectorVarint{Value: v, Hex: hex.EncodeToString(AppendVarint(nil, v))})
	}
	for _, n := range []int{0, 1, 63, 64, 536, 4096, 16383, 16384, MaxTunFrame} {
		want.Messages = append(want.Messages, quicVectorMessage{Len: n, Prefix: hex.EncodeToString(AppendVarint(nil, uint64(n)))})
	}
	for _, tg := range []struct{ name, target string }{
		{"box", TargetBox},
		{"app", AppTarget(signed.BoxKey.Home)},
	} {
		want.Targets = append(want.Targets, quicVectorTarget{Name: tg.name, Target: tg.target,
			Message: hex.EncodeToString(AppendMessage(nil, []byte(tg.target)))})
	}
	for _, m := range signed.Messages {
		b, _ := hex.DecodeString(m.Envelope)
		env, err := DecodeEnvelope(b)
		if err != nil {
			t.Fatal(err)
		}
		want.Datagrams = append(want.Datagrams, quicVectorDatagram{Name: m.Name, Envelope: m.Envelope, Datagram: Datagram(env)})
	}

	out, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if *update {
		if err := os.WriteFile(quicVectorsPath, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(quicVectorsPath)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, out) {
		t.Fatalf("%s is out of date; run go test ./cloudwire -run TestQUICVectors -update", quicVectorsPath)
	}

	// The file says what the rules say, read back through the decoder.
	var file quicVectorFile
	if err := json.Unmarshal(got, &file); err != nil {
		t.Fatal(err)
	}
	for _, v := range file.Varints {
		b, _ := hex.DecodeString(v.Hex)
		n, err := ReadVarint(bytes.NewReader(b))
		if err != nil || n != v.Value {
			t.Fatalf("varint %s reads %d, %v", v.Hex, n, err)
		}
	}
	datagrams := map[string]bool{}
	for _, d := range file.Datagrams {
		datagrams[d.Name] = d.Datagram
	}
	for name, ok := range map[string]bool{"hb": true, "live delta": true, "live full": false, "prices": false,
		"tun down": false, "tun up": false, "roster": false, "event connected": false} {
		if datagrams[name] != ok {
			t.Errorf("%s as a datagram: %v, want %v", name, datagrams[name], ok)
		}
	}
}

func TestMessagesRoundTripOnAStream(t *testing.T) {
	var stream []byte
	msgs := [][]byte{{}, []byte("x"), bytes.Repeat([]byte{7}, 64), bytes.Repeat([]byte{9}, 16384), []byte(TargetBox)}
	for _, m := range msgs {
		stream = AppendMessage(stream, m)
	}
	r := bufio.NewReader(bytes.NewReader(stream))
	for i, m := range msgs {
		got, err := ReadMessage(r, MaxTunFrame)
		if err != nil || !bytes.Equal(got, m) {
			t.Fatalf("message %d: %d bytes, %v", i, len(got), err)
		}
	}
	if _, err := ReadMessage(r, MaxTunFrame); err != io.EOF {
		t.Fatalf("after the last message: %v", err)
	}
}

// A length over the limit is refused before a byte of the message is read,
// so a peer cannot make the reader allocate what it declares.
func TestALengthOverTheLimitIsRefused(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader(AppendVarint(nil, maxVarint)))
	if _, err := ReadMessage(r, MaxTunFrame); !errors.Is(err, ErrQUICMessage) {
		t.Fatalf("a 2^62 length: %v", err)
	}
	cut := AppendMessage(nil, []byte("hello"))[:3]
	if _, err := ReadMessage(bufio.NewReader(bytes.NewReader(cut)), 64); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a message cut short: %v", err)
	}
	if _, err := ReadVarint(bytes.NewReader([]byte{0x40})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a varint cut short: %v", err)
	}
}

func TestTheAppTargetNamesTheHome(t *testing.T) {
	if got := AppTarget("abc"); got != "/v1/app?home=abc" || !strings.HasPrefix(got, AppPath) {
		t.Fatal(got)
	}
}
