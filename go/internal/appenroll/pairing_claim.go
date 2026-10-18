package appenroll

import (
	"crypto/subtle"
	"fmt"
	"time"
)

// RedeemSpokenCode exchanges a local setup code for the full enrollment
// payload's token. It keeps the role and expiry, and replaces the short code
// atomically. The caller must use the box's protected, local HTTP API.
func (i *Identity) RedeemSpokenCode(typed string) ([]byte, time.Time, error) {
	code, err := DecodeSpokenCode(typed)
	i.mu.Lock()
	pending := i.pairing
	if pending == nil || pending.kind != PairingKindSpoken || !i.now().Before(pending.expiresAt) {
		i.mu.Unlock()
		return nil, time.Time{}, ErrBadPairing
	}
	if err != nil || subtle.ConstantTimeCompare(code, pending.code) != 1 {
		pending.attempts++
		if pending.attempts >= MaxPairingAttempts {
			i.pairing = nil
		}
		i.mu.Unlock()
		return nil, time.Time{}, ErrBadPairing
	}

	token := make([]byte, PairingCodeBytes)
	if _, err := i.randRead(token); err != nil {
		i.mu.Unlock()
		return nil, time.Time{}, fmt.Errorf("appenroll: creating enrollment token: %w", err)
	}
	expires := pending.expiresAt
	i.pairing = &pairingCode{code: token, expiresAt: expires, role: pending.role, kind: PairingKindQR}
	i.mu.Unlock()
	i.minted()
	return append([]byte(nil), token...), expires, nil
}
