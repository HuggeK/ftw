package appenroll

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

func TestLocalCodeClaimKeepsRoleExpiryAndSpendsTheShortCode(t *testing.T) {
	id, _ := newIdentity(t)
	now := time.Unix(1_800_000_000, 0)
	id.now = func() time.Time { return now }
	shown, expires, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	var notified int
	id.OnPairingCode(func() { notified++ })
	token, until, err := id.RedeemSpokenCode(shown)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != PairingCodeBytes || !until.Equal(expires) || id.pairing.role != apiauth.RoleViewer {
		t.Fatal("claim changed the role or expiry, or returned a short token")
	}
	pending, _, ok := id.PendingCode()
	if !ok || !bytes.Equal(pending, token) || notified != 1 {
		t.Fatal("Cloud was not told of the new full token")
	}
	if _, _, err := id.RedeemSpokenCode(shown); !errors.Is(err, ErrBadPairing) {
		t.Fatal("short code could be used twice")
	}
	if _, err := id.Authorise(appKey(t), token); err != nil {
		t.Fatalf("full token did not enroll: %v", err)
	}
}

func TestLocalCodeClaimSharesTheGuessLimitAndExpiry(t *testing.T) {
	id, _ := newIdentity(t)
	now := time.Now()
	id.now = func() time.Time { return now }
	shown, _, err := id.MintSpokenCode(apiauth.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	for range MaxPairingAttempts {
		if _, _, err := id.RedeemSpokenCode("not a code"); !errors.Is(err, ErrBadPairing) {
			t.Fatal(err)
		}
	}
	if _, _, err := id.RedeemSpokenCode(shown); !errors.Is(err, ErrBadPairing) {
		t.Fatal("guesses did not burn code")
	}
	shown, until, err := id.MintSpokenCode(apiauth.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	now = until
	if _, _, err := id.RedeemSpokenCode(shown); !errors.Is(err, ErrBadPairing) {
		t.Fatal("expired code claimed")
	}
}

func TestOnlyOneLocalClaimWins(t *testing.T) {
	id, _ := newIdentity(t)
	shown, _, err := id.MintSpokenCode(apiauth.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Go(func() { _, _, err := id.RedeemSpokenCode(shown); results <- err })
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims succeeded, want one", wins)
	}
}
