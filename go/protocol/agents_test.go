package protocol

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The approval vectors are what the phone and FTW Cloud replay: for each
// signed message, its body as it travels, the exact bytes the phone signs
// and a signature by the vector key. Regenerate with
// FTW_WRITE_VECTORS=1 go test ./protocol -run TestApprovalVectors; the
// bytes must not change for an existing case.

type approvalVectors struct {
	Note     string           `json:"note"`
	PubHex   string           `json:"pubHex"`
	BoxKey   string           `json:"boxKeyHex"`
	DeviceID string           `json:"deviceId"`
	Cases    []approvalVector `json:"cases"`
	Offer    offerVector      `json:"offer"`
}

type approvalVector struct {
	What    string `json:"what"`
	BodyHex string `json:"bodyHex"`
	// ContextHex is what the box adds from its own records: the row's key
	// for agent.widen, the stored request (CBOR) for agent.confirm.
	ContextHex string `json:"contextHex,omitempty"`
	SignedHex  string `json:"signedHex"`
	SHA256Hex  string `json:"sha256Hex"`
	SigHex     string `json:"sigHex"`
}

// offerVector is one signed offer: the link fragment's CBOR, what the offer
// key signed, and the key.
type offerVector struct {
	Note      string `json:"note"`
	Kid       string `json:"kid"`
	PubHex    string `json:"pubHex"`
	NowMs     int64  `json:"nowMs"`
	SignedHex string `json:"signedOfferHex"`
	OHex      string `json:"oHex"`
	BytesHex  string `json:"signedBytesHex"`
}

const approvalVectorsPath = "testdata/approval-vectors.json"

// vectorKey is a fixed P-256 key, so the public half in the file never
// changes. It signs nothing outside these tests.
func vectorKey() *ecdsa.PrivateKey {
	d, _ := new(big.Int).SetString("6b2c7e1f0a9d3c4b5e8f7a6d1c2b3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d", 16)
	key := &ecdsa.PrivateKey{D: d}
	key.Curve = elliptic.P256()
	key.X, key.Y = key.Curve.ScalarBaseMult(d.Bytes())
	return key
}

func vectorPub(key *ecdsa.PrivateKey) []byte {
	return elliptic.Marshal(elliptic.P256(), key.X, key.Y) //nolint:staticcheck // SEC1 uncompressed is the wire form.
}

func signRaw(t *testing.T, key *ecdsa.PrivateKey, msg []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, ApprovalSigBytes)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func vectorFor() SignedFor {
	box := make([]byte, 32)
	for i := range box {
		box[i] = byte(i + 1)
	}
	return SignedFor{BoxKey: box, DeviceID: "ownerPh1"}
}

func vectorAgentKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xa0 + i)
	}
	return key
}

// contexts is what the box adds to each signed message from its records.
var contexts = map[string][]byte{}

// vectorRequest is the stored request the confirmation vector approves.
func vectorRequest() Request {
	return Request{ReqID: "AAECAwQFBgcICQoLDA0ODw", Agent: "oKGio6Sl", Op: OpSetMode,
		Args: map[string]any{"mode": "self_consumption"}, Why: "Cheaper tonight",
		AtMs: 1_792_000_000_000, ExpiresAtMs: 1_792_000_000_000 + RequestTTLMs, State: RequestPending}
}

// vectorMessages builds every signed message, each with its signing bytes.
func vectorMessages(t *testing.T, key *ecdsa.PrivateKey) []struct {
	what string
	body any
	sign func(sig []byte) any
	raw  []byte
} {
	t.Helper()
	f := vectorFor()
	const atMs = 1_792_000_000_000
	// The vectors keep the 30-day ends they were cut with: the app checks
	// the same bytes, and the length is only data here.
	const month = 30 * 24 * 60 * 60 * 1000

	keySet := ApprovalKeySet{Pub: vectorPub(key), AtMs: atMs}
	add := AgentAdd{
		Key: vectorAgentKey(), Role: RoleAgent,
		Scopes:      []string{ScopeDispatchHold, ScopeLiveRead, ScopePlanRead},
		Ops:         []string{OpLoadpointHold},
		Caps:        Caps{HoldS: 900, MaxW: 11_000, PerMin: 6},
		ExpiresAtMs: atMs + month,
		Principal:   Principal{Client: "Claude", ClientID: "https://claude.ai/oauth/mcp-client-metadata"},
		AtMs:        atMs,
	}
	support := AgentAdd{
		Key: vectorAgentKey(), Role: RoleSupport,
		Scopes:      []string{ScopeHealthRead, ScopeLogsRead},
		ExpiresAtMs: atMs + SupportMaxMs,
		Principal:   Principal{Client: "Sourceful support", Ticket: "FTW-123"},
		AtMs:        atMs,
	}
	widen := AgentWiden{
		ID: "oKGio6Sl", Epoch: 2,
		Scopes:      []string{ScopeLiveRead, ScopePlanRead},
		Caps:        Caps{PerMin: 6},
		ExpiresAtMs: atMs + month, AtMs: atMs,
	}
	confirm := AgentConfirm{ReqID: vectorRequest().ReqID, Approve: true, AtMs: atMs}

	keySetRaw, err := KeySetBytes(f, keySet)
	if err != nil {
		t.Fatal(err)
	}
	addRaw, err := AddBytes(f, add)
	if err != nil {
		t.Fatal(err)
	}
	supportRaw, err := AddBytes(f, support)
	if err != nil {
		t.Fatal(err)
	}
	widenRaw, err := WidenBytes(f, widen, vectorAgentKey())
	if err != nil {
		t.Fatal(err)
	}
	confirmRaw, err := ConfirmBytes(f, confirm, vectorRequest())
	if err != nil {
		t.Fatal(err)
	}
	type msg = struct {
		what string
		body any
		sign func(sig []byte) any
		raw  []byte
	}
	reqRaw, err := Marshal(vectorRequest())
	if err != nil {
		t.Fatal(err)
	}
	contexts[MsgAgentWiden] = vectorAgentKey()
	contexts[MsgAgentConfirm] = reqRaw
	return []msg{
		{MsgApprovalKeySet, keySet, func(sig []byte) any { m := keySet; m.Sig = sig; return m }, keySetRaw},
		{MsgAgentAdd, add, func(sig []byte) any { m := add; m.Sig = sig; return m }, addRaw},
		{MsgAgentAdd, support, func(sig []byte) any { m := support; m.Sig = sig; return m }, supportRaw},
		{MsgAgentWiden, widen, func(sig []byte) any { m := widen; m.Sig = sig; return m }, widenRaw},
		{MsgAgentConfirm, confirm, func(sig []byte) any { m := confirm; m.Sig = sig; return m }, confirmRaw},
	}
}

func TestApprovalVectors(t *testing.T) {
	key := vectorKey()
	msgs := vectorMessages(t, key)

	if os.Getenv("FTW_WRITE_VECTORS") != "" {
		f := vectorFor()
		out := approvalVectors{
			Note: "docs/agents.md, The approval signature. signedHex is what the phone signs; " +
				"sigHex is ECDSA P-256 over SHA-256 of it, r||s.",
			PubHex: hex.EncodeToString(vectorPub(key)), BoxKey: hex.EncodeToString(f.BoxKey), DeviceID: f.DeviceID,
		}
		for _, m := range msgs {
			sig := signRaw(t, key, m.raw)
			body, err := Marshal(m.sign(sig))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(m.raw)
			out.Cases = append(out.Cases, approvalVector{
				What: m.what, BodyHex: hex.EncodeToString(body), ContextHex: hex.EncodeToString(contexts[m.what]),
				SignedHex: hex.EncodeToString(m.raw), SHA256Hex: hex.EncodeToString(sum[:]), SigHex: hex.EncodeToString(sig),
			})
		}
		out.Offer = makeOfferVector(t)
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.FromSlash(approvalVectorsPath), append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile(filepath.FromSlash(approvalVectorsPath))
	if err != nil {
		t.Fatal(err)
	}
	var vec approvalVectors
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	if vec.PubHex != hex.EncodeToString(vectorPub(key)) {
		t.Fatal("the vector key changed")
	}
	if len(vec.Cases) != len(msgs) {
		t.Fatalf("%d cases, want %d", len(vec.Cases), len(msgs))
	}
	pub := vectorPub(key)
	f := SignedFor{DeviceID: vec.DeviceID}
	f.BoxKey, _ = hex.DecodeString(vec.BoxKey)
	for i, c := range vec.Cases {
		if c.What != msgs[i].what {
			t.Fatalf("case %d is %s, want %s", i, c.What, msgs[i].what)
		}
		if got := hex.EncodeToString(msgs[i].raw); got != c.SignedHex {
			t.Errorf("case %d (%s): signed bytes changed\n got %s\nwant %s", i, c.What, got, c.SignedHex)
		}
		// Rebuild the signed bytes from the body as it travels, which is
		// what the box does.
		body, _ := hex.DecodeString(c.BodyHex)
		ctx, _ := hex.DecodeString(c.ContextHex)
		signed, sig := rebuild(t, c.What, f, body, ctx)
		if hex.EncodeToString(signed) != c.SignedHex {
			t.Errorf("case %d (%s): the body does not rebuild the signed bytes", i, c.What)
		}
		if hex.EncodeToString(sig) != c.SigHex {
			t.Errorf("case %d (%s): the body carries another signature", i, c.What)
		}
		if err := VerifyApproval(pub, signed, sig); err != nil {
			t.Errorf("case %d (%s): %v", i, c.What, err)
		}
		sum := sha256.Sum256(signed)
		if hex.EncodeToString(sum[:]) != c.SHA256Hex {
			t.Errorf("case %d (%s): digest", i, c.What)
		}
	}
}

func rebuild(t *testing.T, what string, f SignedFor, body, ctx []byte) ([]byte, []byte) {
	t.Helper()
	var signed []byte
	var sig []byte
	var err error
	switch what {
	case MsgApprovalKeySet:
		var m ApprovalKeySet
		if err = Unmarshal(body, &m); err == nil {
			signed, err = KeySetBytes(f, m)
			sig = m.Sig
		}
	case MsgAgentAdd:
		var m AgentAdd
		if err = Unmarshal(body, &m); err == nil {
			signed, err = AddBytes(f, m)
			sig = m.Sig
		}
	case MsgAgentWiden:
		var m AgentWiden
		if err = Unmarshal(body, &m); err == nil {
			signed, err = WidenBytes(f, m, ctx)
			sig = m.Sig
		}
	case MsgAgentConfirm:
		var m AgentConfirm
		var req Request
		if err = Unmarshal(body, &m); err == nil {
			if err = Unmarshal(ctx, &req); err == nil {
				signed, err = ConfirmBytes(f, m, req)
				sig = m.Sig
			}
		}
	default:
		t.Fatalf("no rebuild for %s", what)
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed, sig
}

func TestApprovalRefusesWhatItDidNotSign(t *testing.T) {
	key := vectorKey()
	pub := vectorPub(key)
	f := vectorFor()
	add := AgentAdd{Key: vectorAgentKey(), Role: RoleAgent, Scopes: []string{ScopeLiveRead},
		ExpiresAtMs: 2_000, AtMs: 1_000}
	raw, err := AddBytes(f, add)
	if err != nil {
		t.Fatal(err)
	}
	sig := signRaw(t, key, raw)
	if err := VerifyApproval(pub, raw, sig); err != nil {
		t.Fatal(err)
	}

	wider := add
	wider.Scopes = []string{ScopeDispatchHold, ScopeLiveRead}
	widerRaw, _ := AddBytes(f, wider)
	if !errors.Is(VerifyApproval(pub, widerRaw, sig), ErrApprovalSignature) {
		t.Error("a signature over read access passed for control")
	}

	other := f
	other.BoxKey = bytes.Repeat([]byte{9}, 32)
	otherRaw, _ := AddBytes(other, add)
	if !errors.Is(VerifyApproval(pub, otherRaw, sig), ErrApprovalSignature) {
		t.Error("a signature for one box passed at another")
	}

	if !errors.Is(VerifyApproval(pub, raw, sig[:63]), ErrApprovalSignature) {
		t.Error("a short signature passed")
	}
	if !errors.Is(VerifyApproval(pub[:64], raw, sig), ErrApprovalPub) {
		t.Error("a short key passed")
	}
	bad := append([]byte(nil), pub...)
	bad[64] ^= 1
	if !errors.Is(VerifyApproval(bad, raw, sig), ErrApprovalPub) {
		t.Error("a point off the curve passed")
	}
}

func TestApprovalBytesRefuseAnUnsortedList(t *testing.T) {
	f := vectorFor()
	for _, scopes := range [][]string{
		{ScopePlanRead, ScopeLiveRead},
		{ScopeLiveRead, ScopeLiveRead},
	} {
		_, err := AddBytes(f, AgentAdd{Key: vectorAgentKey(), Role: RoleAgent, Scopes: scopes})
		if !errors.Is(err, ErrApprovalUnsorted) {
			t.Errorf("%v: %v", scopes, err)
		}
	}
}

func TestEnvelopeCarriesByOnlyWhenSet(t *testing.T) {
	id := uint32(7)
	plain, err := EncodeEnvelope(Envelope{T: MsgPlanGet, ID: &id})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte("by")) {
		t.Error("an envelope without a staff id names one")
	}
	withBy, err := EncodeEnvelope(Envelope{T: MsgPlanGet, ID: &id, By: "staff-42"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeEnvelope(withBy)
	if err != nil || got.By != "staff-42" {
		t.Fatalf("by did not survive: %+v %v", got, err)
	}
}

// The offer vector's key: fixed like the approval key, and nobody's offer key.
func offerKey() *ecdsa.PrivateKey {
	d, _ := new(big.Int).SetString("2f4e6a8c0b1d3e5f7a9c2b4d6e8f0a1c3e5b7d9f1a2c4e6b8d0f2a4c6e8b0d2f", 16)
	key := &ecdsa.PrivateKey{D: d}
	key.Curve = elliptic.P256()
	key.X, key.Y = key.Curve.ScalarBaseMult(d.Bytes())
	return key
}

const offerNowMs = 1_792_000_000_000

func vectorOffer() GrantOffer {
	return GrantOffer{
		V: GrantOfferVersion, Key: vectorAgentKey(), Role: RoleAgent,
		Client: "Claude", ClientID: "https://claude.ai/oauth/mcp-client-metadata", ClientHost: "claude.ai", Verified: true,
		Scopes: []string{ScopeLiveRead, ScopePlanRead}, Ops: []string{}, TTLS: 30 * 24 * 60 * 60,
		Code: "K7QF-2MXD", ExpMs: offerNowMs + 5*60*1000,
	}
}

func makeOfferVector(t *testing.T) offerVector {
	t.Helper()
	key := offerKey()
	o, err := Marshal(vectorOffer())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := OfferBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Marshal(SignedOffer{O: o, Kid: OfferKid(vectorPub(key)), Sig: signRaw(t, key, msg)})
	if err != nil {
		t.Fatal(err)
	}
	return offerVector{
		Note: "docs/agents.md, The grant offer. The link fragment is base64url of signedOfferHex.",
		Kid:  OfferKid(vectorPub(key)), PubHex: hex.EncodeToString(vectorPub(key)), NowMs: offerNowMs,
		SignedHex: hex.EncodeToString(signed), OHex: hex.EncodeToString(o), BytesHex: hex.EncodeToString(msg),
	}
}

func TestTheOfferVectorOpens(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(approvalVectorsPath))
	if err != nil {
		t.Fatal(err)
	}
	var vec approvalVectors
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	pub, _ := hex.DecodeString(vec.Offer.PubHex)
	keys := map[string][]byte{vec.Offer.Kid: pub}
	signed, _ := hex.DecodeString(vec.Offer.SignedHex)
	var so SignedOffer
	if err := Unmarshal(signed, &so); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOffer(keys, so, vec.Offer.NowMs)
	if err != nil {
		t.Fatal(err)
	}
	if o.Client != "Claude" || o.Home != "" || !o.Verified || o.ClientHost != "claude.ai" {
		t.Errorf("offer %+v", o)
	}
	if want, _ := hex.DecodeString(vec.Offer.BytesHex); !bytes.Equal(mustOfferBytes(t, so.O), want) {
		t.Error("the signed bytes changed")
	}

	if _, err := OpenOffer(keys, so, o.ExpMs); !errors.Is(err, ErrOfferExpired) {
		t.Errorf("at its expiry: %v", err)
	}
	if _, err := OpenOffer(map[string][]byte{"other": pub}, so, vec.Offer.NowMs); !errors.Is(err, ErrOfferSignature) {
		t.Errorf("an unknown kid: %v", err)
	}
	tampered := so
	tampered.O = append([]byte(nil), so.O...)
	tampered.O[len(tampered.O)-1] ^= 1
	if _, err := OpenOffer(keys, tampered, vec.Offer.NowMs); !errors.Is(err, ErrOfferSignature) {
		t.Errorf("a changed offer: %v", err)
	}

	// A support offer from any client but Sourceful's is refused, even signed.
	sup := vectorOffer()
	sup.Role = RoleSupport
	sup.Ticket = "FTW-1"
	so2 := signOffer(t, sup)
	if _, err := OpenOffer(keys2(), so2, offerNowMs); !errors.Is(err, ErrOfferShape) {
		t.Errorf("support from another client: %v", err)
	}
	sup.ClientID = SupportClientID
	if _, err := OpenOffer(keys2(), signOffer(t, sup), offerNowMs); err != nil {
		t.Errorf("support from Sourceful: %v", err)
	}
	// An offer that lives longer than ten minutes is refused.
	long := vectorOffer()
	long.ExpMs = offerNowMs + OfferTTLMs + 1
	if _, err := OpenOffer(keys2(), signOffer(t, long), offerNowMs); !errors.Is(err, ErrOfferExpired) {
		t.Errorf("a long offer: %v", err)
	}
}

func keys2() map[string][]byte {
	return map[string][]byte{OfferKid(vectorPub(offerKey())): vectorPub(offerKey())}
}

func signOffer(t *testing.T, o GrantOffer) SignedOffer {
	t.Helper()
	raw, err := Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return SignedOffer{O: raw, Kid: OfferKid(vectorPub(offerKey())), Sig: signRaw(t, offerKey(), mustOfferBytes(t, raw))}
}

func mustOfferBytes(t *testing.T, o []byte) []byte {
	t.Helper()
	b, err := OfferBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConfirmBindsTheAgentTheOpAndTheArgs(t *testing.T) {
	key := vectorKey()
	pub := vectorPub(key)
	f := vectorFor()
	m := AgentConfirm{ReqID: vectorRequest().ReqID, Approve: true, AtMs: 1}
	raw, err := ConfirmBytes(f, m, vectorRequest())
	if err != nil {
		t.Fatal(err)
	}
	sig := signRaw(t, key, raw)
	for name, edit := range map[string]func(*Request){
		"another agent": func(r *Request) { r.Agent = "someone" },
		"another op":    func(r *Request) { r.Op = OpPlannerPrefsSet },
		"other args":    func(r *Request) { r.Args = map[string]any{"mode": "idle"} },
	} {
		req := vectorRequest()
		edit(&req)
		other, _ := ConfirmBytes(f, m, req)
		if VerifyApproval(pub, other, sig) == nil {
			t.Errorf("%s: the confirmation still verifies", name)
		}
	}
}

func TestByNamesAPersonOrABot(t *testing.T) {
	for by, want := range map[string]bool{
		"staff:anna.k": true, "bot:triage": true, "staff:": false, "admin:x": false, "anna": false,
		"staff:a b": false, "staff:" + strings.Repeat("a", 60): false,
	} {
		if ValidBy(by) != want {
			t.Errorf("%q: want %v", by, want)
		}
	}
}
