package appenroll

// A watch, at the layer that decides what it is (ADR 0009, decisions 4, 5
// and 9).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// addWatch walks one watch through a code its phone minted and returns the
// watch's key and the grant the box stamped on it.
func addWatch(t *testing.T, id *Identity, phone Grant) ([]byte, Grant) {
	t.Helper()
	code, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatalf("MintWatchCode: %v", err)
	}
	key := appKey(t)
	grant, err := id.AuthoriseWatch(key, code)
	if err != nil {
		t.Fatalf("adding a watch: %v", err)
	}
	return key, grant
}

func TestAnOwnerPhoneAddsAWatchThatNamesItAsParent(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)

	key, watch := addWatch(t, id, phone)
	if watch.Role != apiauth.RoleWatch {
		t.Fatalf("the watch is a %q", watch.Role)
	}
	if watch.Parent != phone.DeviceID || watch.ParentRole != apiauth.RoleOwner {
		t.Fatalf("the watch's parent is %q (%q), want %q (owner)", watch.Parent, watch.ParentRole, phone.DeviceID)
	}

	// Its next handshake needs no code, as a phone's does not.
	again, err := id.AuthoriseWatch(key, nil)
	if err != nil {
		t.Fatalf("the watch's second handshake was refused: %v", err)
	}
	if again.DeviceID != watch.DeviceID || again.Parent != phone.DeviceID {
		t.Fatalf("the second handshake found %+v", again)
	}
}

// A viewer cannot add a watch, and neither can a phone the box does not know.
func TestOnlyAnOwnerPhoneMintsAWatchCode(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	viewer := pair(t, id, apiauth.RoleViewer)

	if _, _, err := id.MintWatchCode(viewer.DeviceID, WatchTTL); !errors.Is(err, ErrWatchNeedsOwner) {
		t.Fatalf("a viewer minted a watch code: %v", err)
	}
	if _, _, err := id.MintWatchCode("nosuchid", WatchTTL); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("a stranger minted a watch code: %v", err)
	}
}

// The watch code has a slot of its own: adding a watch leaves an invite that
// somebody is about to scan alone, and the invite leaves the watch code alone.
func TestAWatchCodeDoesNotCancelAPhoneCode(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)

	invite, _, err := id.MintPairingCode(apiauth.RoleViewer, InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	watchCode, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := id.Authorise(appKey(t), invite); err != nil {
		t.Fatalf("the invite died when a watch code was minted: %v", err)
	}
	if _, err := id.AuthoriseWatch(appKey(t), watchCode); err != nil {
		t.Fatalf("the watch code died when the invite was spent: %v", err)
	}
}

// A watch code is spent once, and a phone's door never spends it.
func TestAWatchCodeWorksOnceAndOnlyAtTheWatchDoor(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	code, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := id.Authorise(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a watch code opened a phone's stream: %v", err)
	}
	if _, err := id.AuthoriseWatch(appKey(t), code); err != nil {
		t.Fatalf("the watch code was spent by the stream: %v", err)
	}
	if _, err := id.AuthoriseWatch(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a watch code let a second watch in: %v", err)
	}
	if n := id.AuthorisedCount(); n != 2 {
		t.Fatalf("%d enrolments, want the phone and one watch", n)
	}
}

// Each door admits its own kind and answers the other as a stranger.
func TestAWatchHasNoStreamAndAPhoneNoWatchDoor(t *testing.T) {
	id, _ := newIdentity(t)
	phoneKey := appKey(t)
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := id.Authorise(phoneKey, code)
	if err != nil {
		t.Fatal(err)
	}
	watchKey, _ := addWatch(t, id, phone)

	if _, err := id.Authorise(watchKey, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a watch opened a stream: %v", err)
	}
	if _, err := id.AuthoriseWatch(phoneKey, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a phone came through the watch's door: %v", err)
	}
}

func TestAnExpiredWatchCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	now := time.UnixMilli(1_760_000_000_000)
	id.now = func() time.Time { return now }

	code, expires, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	now = expires.Add(time.Millisecond)
	if _, err := id.AuthoriseWatch(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("an expired watch code was redeemed: %v", err)
	}
}

func TestAWatchCodeIsBurnedByGuessing(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	code, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, PairingCodeBytes)
	for i := 0; i < MaxPairingAttempts; i++ {
		if _, err := id.AuthoriseWatch(appKey(t), wrong); !errors.Is(err, ErrBadPairing) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if _, err := id.AuthoriseWatch(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("the right code still worked after %d wrong guesses: %v", MaxPairingAttempts, err)
	}
}

// The phone must still be an owner when the watch spends the code.
func TestAWatchCodeDiesWithItsPhonesOwnerRole(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	code, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := id.SetRole(phone.DeviceID, apiauth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := id.AuthoriseWatch(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a watch joined under a phone that is no longer an owner: %v", err)
	}
}

// Removing a phone removes its watches, and their keys come back so the
// caller can end their sessions. Removing a watch touches nothing else.
func TestRemovalRunsFromPhoneToWatchAndNeverBack(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	firstKey, first := addWatch(t, id, phone)
	secondKey, second := addWatch(t, id, phone)

	removed, err := id.Revoke(first.DeviceID, OverASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || string(removed[0].Key) != string(firstKey) {
		t.Fatalf("removing a watch removed %+v", removed)
	}
	if _, ok := id.GrantFor(phone.DeviceID); !ok {
		t.Fatal("removing a watch removed its phone")
	}
	if _, ok := id.GrantFor(second.DeviceID); !ok {
		t.Fatal("removing one watch removed another")
	}
	if _, err := id.AuthoriseWatch(firstKey, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a removed watch's next message 1 was answered: %v", err)
	}

	pending, _, err := id.MintWatchCode(phone.DeviceID, WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	removed, err = id.Revoke(phone.DeviceID, OverASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0].ID != phone.DeviceID || removed[1].ID != second.DeviceID {
		t.Fatalf("removing the phone removed %+v, want the phone then its watch", removed)
	}
	if _, ok := id.GrantFor(second.DeviceID); ok {
		t.Fatal("a watch outlived its phone")
	}
	if _, err := id.AuthoriseWatch(secondKey, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a watch whose phone was removed was answered: %v", err)
	}
	for _, d := range id.Roster() {
		if d.ID == second.DeviceID {
			t.Fatal("a removed watch is still in the roster")
		}
	}
	if _, err := id.AuthoriseWatch(appKey(t), pending); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("a watch code outlived the phone that asked for it: %v", err)
	}
}

// A watch is a watch until it is removed; its reach follows its phone.
func TestTheWatchRoleCannotBeGivenOrTakenByARoleChange(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	if err := id.SetRole(watch.DeviceID, apiauth.RoleOwner); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a watch was made an owner: %v", err)
	}
	if err := id.SetRole(phone.DeviceID, apiauth.RoleWatch); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a phone was made a watch: %v", err)
	}
	if _, _, err := id.MintPairingCode(apiauth.RoleWatch, PairingTTL); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a phone's code was minted for a watch: %v", err)
	}
	if _, _, err := id.MintSpokenCode(apiauth.RoleWatch); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a box code was minted for a watch: %v", err)
	}
}

// Demoting the phone narrows its watch: the grant names the parent's new role
// and moves its epoch, so the next roster carries the narrower scopes.
func TestDemotingAPhoneMovesItsWatchsGrant(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	if err := id.SetRole(phone.DeviceID, apiauth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	after, ok := id.GrantFor(watch.DeviceID)
	if !ok {
		t.Fatal("demoting the phone removed its watch")
	}
	if after.ParentRole != apiauth.RoleViewer {
		t.Fatalf("the watch's parent role is %q after the demotion", after.ParentRole)
	}
	if after.Epoch <= watch.Epoch {
		t.Fatalf("the watch's epoch stayed at %d", after.Epoch)
	}
	for _, d := range id.Roster() {
		if d.ID == watch.DeviceID && (d.ParentRole != apiauth.RoleViewer || d.Epoch != after.Epoch) {
			t.Fatalf("the roster has %+v for the watch", d)
		}
	}
}

// The list puts each watch right after its phone.
func TestTheDeviceListPutsAWatchUnderItsPhone(t *testing.T) {
	id, _ := newIdentity(t)
	now := time.UnixMilli(1_760_000_000_000)
	id.now = func() time.Time { return now }

	first := pair(t, id, apiauth.RoleOwner)
	now = now.Add(time.Minute)
	second := pair(t, id, apiauth.RoleOwner)
	now = now.Add(time.Minute)
	_, watch := addWatch(t, id, first)

	rows := id.Devices()
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	got := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	want := []string{second.DeviceID, first.DeviceID, watch.DeviceID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows in order %v, want %v", got, want)
	}
	if rows[2].Parent != first.DeviceID || rows[2].Role != apiauth.RoleWatch || rows[2].LastOwner {
		t.Fatalf("the watch's row is %+v", rows[2])
	}
	if rows[0].Parent != "" || rows[1].Parent != "" {
		t.Fatal("a phone's row names a parent")
	}
}

// A watch is a device in the roster, with its role and its phone's role.
func TestAWatchIsInTheRoster(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	key, watch := addWatch(t, id, phone)

	for _, d := range id.Roster() {
		if d.ID != watch.DeviceID {
			continue
		}
		if d.Role != apiauth.RoleWatch || d.ParentRole != apiauth.RoleOwner || string(d.Key) != string(key) {
			t.Fatalf("the watch's roster entry is %+v", d)
		}
		return
	}
	t.Fatal("the watch is not in the roster")
}

func TestAWatchAndItsParentSurviveARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	key, watch := addWatch(t, id, phone)

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := again.AuthoriseWatch(key, nil)
	if err != nil {
		t.Fatalf("the watch was forgotten across a restart: %v", err)
	}
	if grant.DeviceID != watch.DeviceID || grant.Parent != phone.DeviceID {
		t.Fatalf("after a restart the watch is %+v", grant)
	}
}

// A watch whose phone is missing from the file goes at load rather than
// lingering as a device nobody can narrow.
func TestAWatchWithoutItsPhoneIsDroppedAtLoad(t *testing.T) {
	id, keyPath := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	// Write the file as a crash between the two halves of a removal would
	// have left it: the phone gone, its watch still there.
	path := filepath.Join(filepath.Dir(keyPath), FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id.mu.Lock()
	phoneKey, _ := id.findLocked(phone.DeviceID)
	delete(id.authorised, phoneKey)
	id.mu.Unlock()
	if err := id.save(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) == string(raw) {
		t.Fatal("the test did not change the file")
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.GrantFor(watch.DeviceID); ok {
		t.Fatal("an orphaned watch loaded")
	}
	if n := again.AuthorisedCount(); n != 0 {
		t.Fatalf("%d enrolments after loading an orphan", n)
	}
}
