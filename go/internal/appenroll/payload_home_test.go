package appenroll

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

const testHome = "fjzyr67buzpyncqmjliwgezvde"

// v3 is v2 with the home id as a sixth segment, base64url like the hint.
func TestAV3PayloadNamesTheHome(t *testing.T) {
	id, _ := newIdentity(t)
	id.SetHome(func() string { return testHome })
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	url, err := id.enrollmentURL(code, "192.168.1.42:8443", true)
	if err != nil {
		t.Fatal(err)
	}
	before, fragment, _ := strings.Cut(url, "#")
	if strings.Contains(before, testHome) || strings.Contains(before, "?") {
		t.Fatalf("the home id is outside the fragment: %q", url)
	}
	parts := strings.Split(fragment, ".")
	if len(parts) != 6 || parts[0] != PayloadVersionHome {
		t.Fatalf("segments %q", parts)
	}
	home, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || string(home) != testHome {
		t.Fatalf("segment 5 is %q: %v", home, err)
	}
	// The first five are v2's, byte for byte.
	v2, _ := id.enrollmentURL(code, "192.168.1.42:8443", false)
	_, v2fragment, _ := strings.Cut(v2, "#")
	if strings.Join(parts[1:5], ".") != strings.Join(strings.Split(v2fragment, ".")[1:], ".") {
		t.Fatal("v3 changed one of v2's segments")
	}
	if !bytes.Equal(mustDecode(t, parts[2]), code) {
		t.Fatal("segment 2 is not the code")
	}
}

// A box that is not linked has no home to name, so it shows v2.
func TestAnUnlinkedBoxShowsV2(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, _ := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	for _, home := range []func() string{nil, func() string { return "" }, func() string { return "not a home" }} {
		id.SetHome(home)
		url, err := id.enrollmentURL(code, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, fragment, _ := strings.Cut(url, "#"); !strings.HasPrefix(fragment, PayloadVersion+".") ||
			len(strings.Split(fragment, ".")) != 5 {
			t.Fatalf("payload %q", url)
		}
	}
}

// A linked box gives a new client enough to pair with the relay blocked.
func TestTheLinkedQRNamesItsCloudHome(t *testing.T) {
	id, _ := newIdentity(t)
	id.SetHome(func() string { return testHome })
	code, _, _ := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	url, err := id.EnrollmentURL(code, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "."+base64.RawURLEncoding.EncodeToString([]byte(testHome))) || !strings.Contains(url, "#"+PayloadVersionHome+".") {
		t.Fatalf("the linked QR omits its Cloud home: %q", url)
	}
}
