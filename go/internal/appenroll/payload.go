package appenroll

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The enrollment payload, printed as a QR code.
//
//	https://app.ftw.energy/p#v2.<box_noise_pub>.<pairing_code>.<lan_hint>.<rendezvous_secret>
//	https://app.ftw.energy/p#v3.<box_noise_pub>.<pairing_code>.<lan_hint>.<rendezvous_secret>.<home>
//
// v3 adds the home id at FTW Cloud. The box shows v2 until QRNamesHome says
// otherwise.
//
// Everything after the '#' is a URL fragment, and a fragment is never sent in
// an HTTP request. That is the whole design: the app's trust anchor reaches it
// from the box by QR, link, paste or local code exchange, without a cloud
// server on the way, so a hostile or compelled cloud
// can deny service but cannot present itself as this box. Nothing here may
// ever move into the path or the query.
//
// The reference implementation is srcfl/ftw-webapp
// src/lib/identity/enrollment.ts, and payload_test.go checks this against the
// lengths and alphabet that parser enforces.
const (
	// PayloadVersion is the first segment. v1 had four segments and no
	// rendezvous secret, so an app that knows only v1 must refuse a v2
	// payload rather than parse three quarters of it.
	PayloadVersion = "v2"

	// PayloadVersionHome is v2 with a sixth segment: the home id at FTW
	// Cloud, base64url of its 26 ASCII characters, so a new phone knows
	// where to dial before any session with the box. Both the web client
	// and iPhone read this form.
	PayloadVersionHome = "v3"

	// PayloadOrigin is where the app is served. It is also the passkey RP
	// ID, which is why it is one constant and not two.
	PayloadOrigin = "https://app.ftw.energy"
	PayloadPath   = "/p"

	// MaxLANHintChars bounds the hint, because the app turns it into a
	// connection target. A bound, not a budget.
	MaxLANHintChars = 64
)

// QRNamesHome puts the Cloud home id in the QR, as v3. The web client needs
// it to pair through Cloud; iPhone also reads v3. An unlinked box keeps v2.
const QRNamesHome = true

// SetHome tells enrolment where to find the home id at FTW Cloud, for a QR
// that names it. fn returns "" while the box is not linked.
func (i *Identity) SetHome(fn func() string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.home = fn
}

// EnrollmentURL builds the payload for the QR code.
//
// lanHint is where the box believes it can be reached on the local network,
// and is allowed to be empty: a box that does not know its own address should
// say nothing rather than guess. The Cloud home id does not depend on it.
func (i *Identity) EnrollmentURL(code []byte, lanHint string) (string, error) {
	return i.enrollmentURL(code, lanHint, QRNamesHome)
}

// enrollmentURL builds v2, or v3 with the home id when withHome is set and
// the box is linked.
func (i *Identity) enrollmentURL(code []byte, lanHint string, withHome bool) (string, error) {
	if len(code) != PairingCodeBytes {
		return "", fmt.Errorf("appenroll: pairing code is %d bytes, need %d", len(code), PairingCodeBytes)
	}
	if len(lanHint) > MaxLANHintChars {
		return "", fmt.Errorf("appenroll: lan hint is %d characters, at most %d", len(lanHint), MaxLANHintChars)
	}
	// The app refuses anything outside printable ASCII, and a hint it refuses
	// costs a re-scan the user cannot diagnose. Caught here instead.
	for _, r := range lanHint {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("appenroll: lan hint is not a plain address")
		}
	}

	enc := base64.RawURLEncoding
	segments := []string{
		PayloadVersion,
		enc.EncodeToString(i.StaticKey().Public()),
		enc.EncodeToString(code),
		enc.EncodeToString([]byte(lanHint)),
		enc.EncodeToString(i.RendezvousSecret()),
	}
	if home := i.homeID(); withHome && cloudwire.ValidHome(home) {
		segments[0] = PayloadVersionHome
		segments = append(segments, enc.EncodeToString([]byte(home)))
	}
	return PayloadOrigin + PayloadPath + "#" + strings.Join(segments, "."), nil
}

func (i *Identity) homeID() string {
	i.mu.Lock()
	fn := i.home
	i.mu.Unlock()
	if fn == nil {
		return ""
	}
	return fn()
}
