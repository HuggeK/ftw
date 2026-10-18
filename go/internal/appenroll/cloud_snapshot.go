package appenroll

import (
	"bytes"
	"time"
)

type InviteHint struct {
	Hash    []byte
	Expires time.Time
}

type CloudSnapshot struct {
	Devices        []RosterEntry
	Noise          []byte
	PairingCode    []byte
	PairingExpires time.Time
	Invites        []InviteHint
}

// CloudSnapshot reads devices and bootstrap hints under the same lock. A
// claim must appear as either its hint or its new agent, never neither.
func (i *Identity) CloudSnapshot() CloudSnapshot {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := CloudSnapshot{Devices: i.rosterLocked(), Noise: i.static.Public()}
	if p := i.pairing; p != nil && p.kind == PairingKindQR && i.now().Before(p.expiresAt) {
		s.PairingCode, s.PairingExpires = bytes.Clone(p.code), p.expiresAt
	}
	now, ok := i.wallLocked()
	if ok {
		for _, v := range i.invites {
			if i.inviteViewLocked(v, now, true).State == "pending" {
				s.Invites = append(s.Invites, InviteHint{Hash: bytes.Clone(v.Hash), Expires: i.now().Add(time.Duration(v.ClaimUntilMs-now) * time.Millisecond)})
			}
		}
	}
	return s
}
