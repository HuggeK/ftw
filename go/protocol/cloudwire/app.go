package cloudwire

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// The app session: a phone reads its home from FTW Cloud.
//
// The phone opens wss://cloud.ftw.energy/v1/app?home=<home id> and runs
// Noise_IK_25519_ChaChaPoly_SHA256 as initiator, with the service's Noise key
// as the responder's static key and AppPrologue(home) as the prologue. The
// query only says which home to look in: a wrong one fails the handshake,
// because the prologue binds it. Message 1 carries a now envelope, message 2
// the snapshot, and after that each box envelope the device may read rides,
// unchanged, in an unpadded lane 1 frame inside a Noise transport message,
// exactly as appwire.Transport writes one. The phone's session with its box
// rides the same transport as tun (tunnel.go).
//
// Every envelope between phone and service has an empty sig. Only box
// envelopes carry signatures, and the phone checks them with the box key it
// learned from the box over its own Noise session, never from the service.

// AppPrologueTag starts the prologue; the home id follows it.
const AppPrologueTag = "ftw.cloud.v1:"

// AppHomeParam is the query parameter that names the home.
const AppHomeParam = "home"

// Phone to service. The service acts on these and ignores every other type.
const (
	// MsgNow is message 1's payload. Watch semantics follow.
	MsgNow = "now"
	// MsgSuspend and MsgUnsuspend are an owner's; they only narrow access.
	MsgSuspend   = "suspend"
	MsgUnsuspend = "unsuspend"
	// MsgWatch (shared with the downlink) changes whether this phone is
	// looking: hz 1 while it can see the home, 0 when it cannot.
	// MsgTun (shared with the uplink, tunnel.go) carries the phone's
	// session with its box, as AppTun, both ways.
)

// Service to phone.
const (
	// MsgSnapshot is message 2's payload.
	MsgSnapshot = "snapshot"
)

// Close codes on /v1/app. A phone reads them to decide whether to retry.
const (
	// CloseProtocol: the phone sent something this protocol does not allow.
	CloseProtocol = 4400
	// CloseDenied is every refusal: no such home, a device not in the roster,
	// a suspended device, an expired roster, or a device that may neither
	// read nor open a tunnel. It does not say which.
	CloseDenied = 4403
	// CloseReplaced: a newer session from the same device took over.
	CloseReplaced = 4408
	// CloseChanged: the roster changed what this device may read. A phone
	// reconnects at once to learn its new access.
	CloseChanged = 4409
	// CloseBusy: the home has as many sessions open as the service allows.
	// The relay uses the same number for the same thing.
	CloseBusy = 4429
)

// Hz values for now and watch.
const (
	HzLooking = 1
	HzAway    = 0
)

// Now is message 1: whether the phone is looking.
type Now struct {
	Hz int `cbor:"hz"`
	// Pair is PairingHash of the code a new phone scanned. It admits a key
	// the roster does not list yet, once, while the roster lists the hash.
	Pair []byte `cbor:"pair,omitempty"`
	// Tun is the first message of the phone's session with its box,
	// handshake message 1. The service forwards it as soon as it has
	// admitted the phone, so the box session is up one round trip after
	// TLS rather than two.
	Tun *AppTun `cbor:"tun,omitempty"`
}

// Suspend names the roster device an owner suspends or lifts a suspension
// from.
type Suspend struct {
	DeviceID string `cbor:"deviceId"`
}

// Snapshot is message 2: for each class the device may read, the newest
// envelope the service holds, the newest live that carried sources and the
// newest hb, in rising seq, as the box sent them. LastSeenMs and Online are
// the service's own word, unsigned, and never shown as a reading.
//
// Envelopes that do not fit in one Noise message follow message 2 as
// ordinary frames, so More counts them.
type Snapshot struct {
	Envelopes  []cbor.RawMessage `cbor:"envelopes"`
	LastSeenMs int64             `cbor:"lastSeenMs"`
	Online     bool              `cbor:"online"`
	More       int               `cbor:"more"`
}

// AppPrologue is the Noise prologue for one home's app session.
func AppPrologue(home string) []byte {
	return append([]byte(AppPrologueTag), home...)
}

// ValidHome reports whether s has a home id's shape: 26 characters of
// lowercase base32.
func ValidHome(s string) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// Unsigned wraps a body the phone or service says itself: an envelope with an
// empty sig.
func Unsigned(t string, body any) ([]byte, error) {
	b, err := Marshal(body)
	if err != nil {
		return nil, err
	}
	return Envelope{T: t, B: b}.Encode()
}

// SnapshotPayload is message 2's payload around envelopes already encoded.
// They are carried as they are: the service never re-encodes a box message.
func SnapshotPayload(envelopes [][]byte, lastSeenMs int64, online bool, more int) ([]byte, error) {
	raw := make([]cbor.RawMessage, len(envelopes))
	for i, e := range envelopes {
		if _, err := DecodeEnvelope(e); err != nil {
			return nil, fmt.Errorf("cloudwire: snapshot entry %d: %w", i, err)
		}
		raw[i] = e
	}
	return Unsigned(MsgSnapshot, Snapshot{Envelopes: raw, LastSeenMs: lastSeenMs, Online: online, More: more})
}

// ParseSnapshot reads message 2. Each entry comes back as its exact bytes.
func ParseSnapshot(payload []byte) (Snapshot, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return Snapshot{}, err
	}
	if env.T != MsgSnapshot {
		return Snapshot{}, fmt.Errorf("%w: message 2 is %q, not a snapshot", ErrEnvelope, env.T)
	}
	var s Snapshot
	if err := Unmarshal(env.B, &s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
