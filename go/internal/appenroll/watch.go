package appenroll

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// WatchTTL is how long a watch code stays usable. The phone hands the code to
// its watch at once, so the life of an owner's code is ample.
const WatchTTL = PairingTTL

// MintWatchCode issues a single-use code that lets one watch in, bound to the
// owner phone that asked for it (ADR 0009, decision 4).
//
// The code has its own slot. Minting it replaces a pending watch code and
// leaves a phone's pending code alone, so adding a watch never cancels an
// invite somebody is about to scan.
//
// The phone must be an owner now. The API checks that too, and only over a
// session with a fresh passkey step-up; the check here keeps the rule a
// property of the box rather than of one route.
func (i *Identity) MintWatchCode(parentID string, ttl time.Duration) ([]byte, time.Time, error) {
	code := make([]byte, PairingCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return nil, time.Time{}, fmt.Errorf("appenroll: minting a watch code: %w", err)
	}
	expires := i.now().Add(ttl)

	i.mu.Lock()
	defer i.mu.Unlock()
	parentKey, parent := i.findLocked(parentID)
	if parent == nil {
		return nil, time.Time{}, ErrUnknownDevice
	}
	if parent.role != apiauth.RoleOwner {
		return nil, time.Time{}, ErrWatchNeedsOwner
	}
	i.watchPairing = &pairingCode{
		code: code, expiresAt: expires, role: apiauth.RoleWatch, kind: PairingKindQR, parent: parentKey,
	}
	return append([]byte(nil), code...), expires, nil
}

// AuthoriseWatch decides whether a finished exchange handshake may become a
// watch's session. It is Authorise for the watch's door.
//
// A watch already on the list needs no code. Anything else needs the live
// watch code, and spending it stores the watch's key with role watch and the
// phone that minted the code as its parent. A phone's key answers as a
// stranger: phones have their stream.
//
// The code is spent, timed and counted as a phone's code is. The parent must
// still be an owner when the code is spent; a phone demoted or removed since
// it asked burns the code instead.
func (i *Identity) AuthoriseWatch(watchStatic, code []byte) (Grant, error) {
	key := base64.RawURLEncoding.EncodeToString(watchStatic)

	i.mu.Lock()
	if meta := i.authorised[key]; meta != nil {
		if meta.role != apiauth.RoleWatch || i.authorised[meta.parent] == nil {
			i.mu.Unlock()
			return Grant{}, ErrNoPairing
		}
		meta.lastSeenMs = i.now().UnixMilli()
		grant := i.grantLocked(key, meta)
		i.mu.Unlock()
		_ = i.save()
		return grant, nil
	}
	if len(code) == 0 {
		i.mu.Unlock()
		return Grant{}, ErrNoPairing
	}
	pairing := i.watchPairing
	if pairing == nil || !i.now().Before(pairing.expiresAt) {
		i.mu.Unlock()
		return Grant{}, ErrBadPairing
	}
	if subtle.ConstantTimeCompare(code, pairing.code) != 1 {
		pairing.attempts++
		if pairing.attempts >= MaxPairingAttempts {
			i.watchPairing = nil
		}
		i.mu.Unlock()
		return Grant{}, ErrBadPairing
	}
	if parent := i.authorised[pairing.parent]; parent == nil || parent.role != apiauth.RoleOwner {
		i.watchPairing = nil
		i.mu.Unlock()
		return Grant{}, ErrBadPairing
	}

	now := i.now().UnixMilli()
	meta := &appMeta{
		addedAtMs: now, lastSeenMs: now, role: apiauth.RoleWatch, epoch: firstEpoch, parent: pairing.parent,
	}
	i.authorised[key] = meta
	i.watchPairing = nil
	grant := i.grantLocked(key, meta)
	i.mu.Unlock()

	err := i.save()
	i.changed()
	return grant, err
}
