package appenroll

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The roster lists the QR code on the screen and nothing else: not a spoken
// code, not a watch code, not one spent or expired.
func TestPendingIsTheLiveQRCodeOnly(t *testing.T) {
	id, _ := newIdentity(t)
	now := time.Now()
	id.now = func() time.Time { return now }
	if _, _, ok := id.PendingCode(); ok {
		t.Fatal("a code is pending before any was minted")
	}

	code, expires, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	got, until, ok := id.PendingCode()
	if !ok || !bytes.Equal(got, code) || !until.Equal(expires) {
		t.Fatalf("pending %x until %v, minted %x until %v", got, until, code, expires)
	}

	now = expires
	if _, _, ok := id.PendingCode(); ok {
		t.Fatal("an expired code is still pending")
	}
	now = time.Now()

	if _, _, err := id.MintSpokenCode(apiauth.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := id.PendingCode(); ok {
		t.Fatal("a spoken code is listed")
	}

	code, _, _ = id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if _, err := id.Authorise(appKey(t), code); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := id.PendingCode(); ok {
		t.Fatal("a spent code is still pending")
	}
}

// Minting a phone's code is heard about, so the roster lists it while it is
// on the screen. It is not a roster change in itself.
func TestMintingIsHeardApartFromChanges(t *testing.T) {
	id, _ := newIdentity(t)
	minted, changes := 0, 0
	id.OnPairingCode(func() { minted++ })
	id.OnChange(func() { changes++ })
	if _, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL); err != nil {
		t.Fatal(err)
	}
	if _, _, err := id.MintSpokenCode(apiauth.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if minted != 2 || changes != 0 {
		t.Fatalf("%d mints heard, %d changes", minted, changes)
	}
}

// FTW Cloud names a device as the box does, so the device id the service
// stamps on a tun is the one the box admits it under.
func TestTheServiceNamesDevicesAsTheBoxDoes(t *testing.T) {
	key := appKey(t)
	if got, want := cloudwire.DeviceID(key), deviceID(base64.RawURLEncoding.EncodeToString(key)); got != want {
		t.Fatalf("cloudwire says %q, the box %q", got, want)
	}
}
