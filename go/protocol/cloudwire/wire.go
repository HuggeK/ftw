// Package cloudwire is the wire format between an FTW box and FTW Cloud: the
// unpadded frame, the signed envelope, the ids, the message bodies and the
// scope each class belongs to. docs/protocol-cloud.md is the normative text;
// this package and its golden vectors hold the bytes.
//
// FTW Cloud reads what the box sends and never chooses an action. Of the six
// message types the service may send the box, five ask for nothing beyond a
// faster cadence, a resend or a receipt. The sixth, tun, carries a phone's own
// Noise session with the box, which the service can neither read nor forge;
// the box judges what is inside as it judges the same session over the relay.
//
// Like the rest of go/protocol, this package imports nothing from the box, so
// the service that imports it links no control, planner or storage code.
package cloudwire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// Fixed strings and sizes. Each one is part of what both ends sign or check,
// so none of them is configurable.
const (
	// Version is byte 0 of every frame and the v of challenge and hello.
	Version = 1
	// Lane is byte 1 of every frame. FTW Cloud uses lane 1 without the app
	// protocol's bulk buckets, because lane 0 is always padded.
	Lane = 1
	// HeaderBytes is ver, lane, flags, rsvd and a u16 length.
	HeaderBytes = 6
	// MaxPayload is what the u16 length field can describe.
	MaxPayload = 0xffff
	// MaxServiceFrame is the largest message but tun the service may send
	// the box, and MaxTunFrame bounds a tun. Anything larger closes the
	// uplink.
	MaxServiceFrame = 4096

	// SignTag starts every signed input: "ftw.cloud.v1" and a zero byte.
	SignTag = "ftw.cloud.v1\x00"
	// HomeTag starts the hash a home id is cut from.
	HomeTag = "ftw.cloud.home.v1"
	// EKMLabel is the TLS exporter label both ends bind the handshake to.
	// RFC 5705 asks for EXPERIMENTAL on a label nobody registered.
	EKMLabel = "EXPERIMENTAL-ftw-cloud-v1"
	// EKMBytes is the length of the exporter value.
	EKMBytes = 32

	// SigBytes is an ES256 signature as r ‖ s, each 32 bytes big-endian.
	SigBytes = 64
	// PublicKeyBytes is an uncompressed SEC1 P-256 point.
	PublicKeyBytes = 65
	// NonceBytes is the size of challenge.nonce and hello.boxNonce.
	NonceBytes = 32
	// BootBytes is the size of the per-start boot id.
	BootBytes = 16
	// GrantIDBytes is the size of a grant id.
	GrantIDBytes = 16
	// GrantKeyBytes is the size of the key opaque ids are cut with.
	GrantKeyBytes = 32
)

// Errors a receiver acts on by closing the connection. They never cross the
// wire.
var (
	ErrFrame     = errors.New("cloudwire: not an FTW Cloud frame")
	ErrEnvelope  = errors.New("cloudwire: not an FTW Cloud envelope")
	ErrSignature = errors.New("cloudwire: signature does not verify")
	ErrKey       = errors.New("cloudwire: not a P-256 public key")
)

// --------------------------------------------------------------------------
// CBOR
// --------------------------------------------------------------------------

// Bodies are core-deterministic CBOR with text keys, so a body encoded twice
// from equal values is equal bytes. A nil slice or map is an empty one on the
// wire, so "no sources are blocking dispatch" is [] and never null; a nil
// pointer is null, which is how a field says "not known".
var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	opts := cbor.CoreDetEncOptions()
	opts.NilContainers = cbor.NilContainerAsEmpty
	var err error
	if encMode, err = opts.EncMode(); err != nil {
		panic(err)
	}
	// Unknown keys are ignored, so a newer box works with an older service.
	// A duplicate key is two claims about one field and is refused.
	if decMode, err = (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}).DecMode(); err != nil {
		panic(err)
	}
}

// Marshal encodes a body deterministically.
func Marshal(v any) ([]byte, error) {
	b, err := encMode.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cloudwire: encode body: %w", err)
	}
	return b, nil
}

// Unmarshal decodes a body. Unknown keys are dropped without complaint.
func Unmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return fmt.Errorf("%w: empty body", ErrEnvelope)
	}
	if err := decMode.Unmarshal(b, v); err != nil {
		return fmt.Errorf("cloudwire: decode body: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------------
// Frames
// --------------------------------------------------------------------------

// EncodeFrame puts one envelope in one frame: the app protocol's layout on
// lane 1, flags 0, and no padding, so the message is exactly 6 + len bytes.
func EncodeFrame(envelope []byte) ([]byte, error) {
	if len(envelope) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes do not fit the length field", ErrFrame, len(envelope))
	}
	out := make([]byte, HeaderBytes+len(envelope))
	out[0] = Version
	out[1] = Lane
	binary.BigEndian.PutUint16(out[4:6], uint16(len(envelope)))
	copy(out[HeaderBytes:], envelope)
	return out, nil
}

// DecodeFrame returns the envelope bytes of one frame. It is strict where the
// app protocol is lenient: lane 0, a flag, a reserved bit or a byte after the
// declared length is not a frame this protocol sends, so it is refused.
func DecodeFrame(msg []byte) ([]byte, error) {
	switch {
	case len(msg) < HeaderBytes:
		return nil, fmt.Errorf("%w: %d bytes", ErrFrame, len(msg))
	case msg[0] != Version:
		return nil, fmt.Errorf("%w: version %d", ErrFrame, msg[0])
	case msg[1] != Lane:
		return nil, fmt.Errorf("%w: lane %d", ErrFrame, msg[1])
	case msg[2] != 0 || msg[3] != 0:
		return nil, fmt.Errorf("%w: flags %#x %#x", ErrFrame, msg[2], msg[3])
	}
	if n := int(binary.BigEndian.Uint16(msg[4:6])); n != len(msg)-HeaderBytes {
		return nil, fmt.Errorf("%w: declares %d bytes, carries %d", ErrFrame, n, len(msg)-HeaderBytes)
	}
	return msg[HeaderBytes:], nil
}

// --------------------------------------------------------------------------
// Envelope
// --------------------------------------------------------------------------

// Envelope is one message: {t, b, sig}. B is the body's exact bytes, carried
// as a byte string so the signature covers what was sent and the service can
// forward it unchanged. Sig is empty on everything the box did not sign
// except hello_ok and purged, which the service signs.
type Envelope struct {
	T   string
	B   []byte
	Sig []byte
}

// Envelope keys in core-deterministic order: shorter keys first, then bytes.
var (
	keyB   = []byte{0x61, 'b'}
	keyT   = []byte{0x61, 't'}
	keySig = []byte{0x63, 's', 'i', 'g'}
)

// Encode writes the envelope as a three-entry map. Written by hand so an
// empty signature is an empty byte string, never null.
func (e Envelope) Encode() ([]byte, error) {
	if e.T == "" {
		return nil, fmt.Errorf("%w: no type", ErrEnvelope)
	}
	t, err := encMode.Marshal(e.T)
	if err != nil {
		return nil, err
	}
	b, err := encMode.Marshal(nonNil(e.B))
	if err != nil {
		return nil, err
	}
	sig, err := encMode.Marshal(nonNil(e.Sig))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(keyB)+len(b)+len(keyT)+len(t)+len(keySig)+len(sig))
	out = append(out, 0xa3)
	out = append(out, keyB...)
	out = append(out, b...)
	out = append(out, keyT...)
	out = append(out, t...)
	out = append(out, keySig...)
	out = append(out, sig...)
	return out, nil
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// DecodeEnvelope parses one envelope. t must be text and b and sig byte
// strings; anything else is not an envelope. Unknown keys are ignored.
func DecodeEnvelope(raw []byte) (Envelope, error) {
	var fields map[string]cbor.RawMessage
	if err := decMode.Unmarshal(raw, &fields); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrEnvelope, err)
	}
	var e Envelope
	if err := decodeKey(fields, "t", &e.T); err != nil {
		return Envelope{}, err
	}
	if err := decodeKey(fields, "b", &e.B); err != nil {
		return Envelope{}, err
	}
	if err := decodeKey(fields, "sig", &e.Sig); err != nil {
		return Envelope{}, err
	}
	if e.T == "" {
		return Envelope{}, fmt.Errorf("%w: empty type", ErrEnvelope)
	}
	return e, nil
}

func decodeKey[T string | []byte](fields map[string]cbor.RawMessage, key string, out *T) error {
	raw, ok := fields[key]
	if !ok {
		return fmt.Errorf("%w: no %q", ErrEnvelope, key)
	}
	// Major type 3 is text, 2 is a byte string. Checked by hand because the
	// decoder would otherwise turn a text string into bytes or the reverse,
	// and an envelope whose body is text is not the envelope that was signed.
	want := byte(0x40)
	if _, isText := any(*out).(string); isText {
		want = 0x60
	}
	if len(raw) == 0 || raw[0]&0xe0 != want {
		return fmt.Errorf("%w: %q has the wrong type", ErrEnvelope, key)
	}
	if err := decMode.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrEnvelope, key, err)
	}
	return nil
}

// --------------------------------------------------------------------------
// Signatures
// --------------------------------------------------------------------------

// SignedInput is what an envelope's signature covers: SignTag, then the
// deterministic CBOR array [home, t, b]. The home id is inside, so a message
// signed for one home never verifies for another.
func SignedInput(home, t string, b []byte) ([]byte, error) {
	arr, err := encMode.Marshal([]any{home, t, nonNil(b)})
	if err != nil {
		return nil, err
	}
	return append([]byte(SignTag), arr...), nil
}

// Seal encodes body, signs it for home with key and returns the envelope.
// A nil key leaves the signature empty, which is how the service sends
// everything but hello_ok and purged.
func Seal(key *ecdsa.PrivateKey, home, t string, body any) (Envelope, error) {
	b, err := Marshal(body)
	if err != nil {
		return Envelope{}, err
	}
	return SealBytes(key, home, t, b)
}

// SealBytes is Seal for a body that is already encoded.
func SealBytes(key *ecdsa.PrivateKey, home, t string, b []byte) (Envelope, error) {
	env := Envelope{T: t, B: b}
	if key == nil {
		return env, nil
	}
	sig, err := Sign(rand.Reader, key, home, t, b)
	if err != nil {
		return Envelope{}, err
	}
	env.Sig = sig
	return env, nil
}

// Sign returns the ES256 signature over SignedInput as r ‖ s.
func Sign(random io.Reader, key *ecdsa.PrivateKey, home, t string, b []byte) ([]byte, error) {
	input, err := SignedInput(home, t, b)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(input)
	r, s, err := ecdsa.Sign(random, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("cloudwire: sign: %w", err)
	}
	sig := make([]byte, SigBytes)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, nil
}

// Verify checks an envelope's signature for home, over the bytes received.
func Verify(pub *ecdsa.PublicKey, home string, e Envelope) error {
	if pub == nil || len(e.Sig) != SigBytes {
		return ErrSignature
	}
	input, err := SignedInput(home, e.T, e.B)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(input)
	r := new(big.Int).SetBytes(e.Sig[:32])
	s := new(big.Int).SetBytes(e.Sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return ErrSignature
	}
	return nil
}

// --------------------------------------------------------------------------
// Keys and ids
// --------------------------------------------------------------------------

// PublicKeyBytesOf is a key's 65-byte uncompressed point.
func PublicKeyBytesOf(pub *ecdsa.PublicKey) ([]byte, error) {
	b, err := pub.Bytes()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKey, err)
	}
	return b, nil
}

// ParsePublicKey reads a 65-byte uncompressed P-256 point.
func ParsePublicKey(b []byte) (*ecdsa.PublicKey, error) {
	if len(b) != PublicKeyBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrKey, len(b))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKey, err)
	}
	return pub, nil
}

var homeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// HomeID is the home's name at the service: lowercase base32 without padding
// of the first 16 bytes of SHA-256(HomeTag ‖ pk), 26 characters. The id is a
// hash of the key, so a second box cannot claim a home that is not its own.
func HomeID(pk []byte) string {
	sum := sha256.Sum256(append([]byte(HomeTag), pk...))
	return strings.ToLower(homeEncoding.EncodeToString(sum[:16]))
}

// SourceID is a source's opaque id: the first 8 bytes of
// HMAC-SHA256(grantKey, "src\0" ‖ driver name), base64url without padding.
// The service sees a stable id per grant and never the driver's name.
func SourceID(grantKey []byte, driver string) string {
	return opaqueID(grantKey, "src\x00", driver)
}

// LoadpointID is a loadpoint's opaque id, cut the same way with "lp\0".
func LoadpointID(grantKey []byte, loadpoint string) string {
	return opaqueID(grantKey, "lp\x00", loadpoint)
}

// CarID is a car's opaque id, cut the same way with "car\0" from its
// 128-bit id. The id is the same on each of the owner's boxes; the opaque id
// differs per grant, so the service cannot follow a car from home to home.
func CarID(grantKey []byte, car string) string {
	return opaqueID(grantKey, "car\x00", car)
}

func opaqueID(key []byte, tag, name string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(tag))
	mac.Write([]byte(name))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:8])
}
