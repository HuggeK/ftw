package cloudwire

import (
	"crypto/sha256"
	"encoding/base64"

	"github.com/srcfl/ftw/go/protocol/appwire"
)

// The tunnel: a phone's own session with its box, carried by FTW Cloud.
// docs/protocol-cloud.md, "Tunnel", is the normative text.
//
// The phone sends tun {sid, d} on its /v1/app session. d is one message of
// the phone's session with the box, ftw.session.v1 (Noise IK to the box's
// static key), byte for byte as it would cross the relay. The service adds
// the device id of the roster entry its own handshake matched and sends
// tun {dev, sid, d} down /v1/box. The box answers tun {dev, sid, d} up, and
// the service hands {sid, d} to that device's session. The service never
// takes dev from a phone. It can drop or delay d; it cannot read or forge it,
// because only the phone and the box hold the session's keys, and Noise
// transport refuses a replay.
//
// An empty d ends the session it names, in either direction: the phone
// closing it, the box dropping it, or the service saying that the other end
// is gone.

// MsgTun is the tunnel message on both sockets.
const MsgTun = "tun"

// CapTun in hello.caps says the box takes tun. The service sends tun to no
// other box: an older one closes the uplink on a type it does not know.
const CapTun = MsgTun

// Sizes. The largest d is one transport message around the app protocol's
// largest bulk frame. A handshake message is far smaller.
const (
	TunDataMax = 16384 + appwire.TransportOverhead
	// MaxTunFrame bounds one tun frame on either socket: d, the device id,
	// the sid and the envelope around them.
	MaxTunFrame = TunDataMax + 256
)

// The service's limits on tun, per home and per direction: a burst, then a
// rate. Past them it drops what is over; at twice them it closes. The box
// paces its bulk under them and never meets them.
const (
	TunMsgBurst       = 240
	TunMsgsPerSecond  = 60
	TunByteBurst      = 4 << 20
	TunBytesPerSecond = 512 << 10
)

// Tun is the tunnel message on /v1/box, both ways.
type Tun struct {
	// Dev is the roster device id whose session this is. The service sets
	// it from the key its own handshake with the phone proved.
	Dev string `cbor:"dev"`
	// Sid is the phone's number for one session with the box, drawn at
	// random for each new session, so that the late end of an older one
	// can never end it. A new sid from the same device starts a new
	// session and ends the older one.
	Sid uint32 `cbor:"sid"`
	// D is one message of that session, or empty to end it.
	D []byte `cbor:"d"`
}

// AppTun is the tunnel message on /v1/app, both ways. It has no device id:
// the service knows the device from the handshake, and ignores a dev a phone
// adds.
type AppTun struct {
	Sid uint32 `cbor:"sid"`
	D   []byte `cbor:"d"`
}

// Ended reports whether the message ends its session.
func (t Tun) Ended() bool { return len(t.D) == 0 }

// DeviceID is the box's name for a paired device: the first 8 characters of
// its Noise static key in base64url without padding. The roster's ids are cut
// this way, and the service names a device it admitted for pairing the same
// way before the roster lists it.
func DeviceID(key []byte) string {
	s := base64.RawURLEncoding.EncodeToString(key)
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// PairingTag starts the hash a pending pairing is listed by.
const PairingTag = "ftw.cloud.pair.v1\x00"

// PairingHashBytes is the length of a pairing hash.
const PairingHashBytes = sha256.Size

// PairingHash is what the roster lists for a pairing code the box shows, and
// what a new phone proves in now.pair: SHA-256(PairingTag ‖ home ‖ code).
// The service never sees the code itself, so it cannot pair a key of its own
// with the box; the box still checks the code inside the phone's Noise
// handshake. Only QR codes are listed. A spoken code has 40 bits, and its
// hash would let the service find it offline.
func PairingHash(home string, code []byte) []byte {
	h := sha256.New()
	h.Write([]byte(PairingTag))
	h.Write([]byte(home))
	h.Write(code)
	return h.Sum(nil)
}
