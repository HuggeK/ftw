package appenroll

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"sort"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

type storedInvite struct {
	protocol.AgentInvite
	Hash        []byte `json:"hash"`
	RequestHash []byte `json:"requestHash"`
	ApprovedBy  string `json:"approvedBy"`
	AgentKey    string `json:"agentKey,omitempty"`
	OfferHash   []byte `json:"offerHash,omitempty"`
}

func (i *Identity) SetAgentOfferKeys(keys map[string][]byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.offerKeys = make(map[string][]byte, len(keys))
	for k, v := range keys {
		i.offerKeys[k] = bytes.Clone(v)
	}
}

func (i *Identity) inviteOwnerLocked(by string) bool {
	if by == LANApprover {
		return true
	}
	m := i.authorised[by]
	return m != nil && m.role == apiauth.RoleOwner && m.agent == nil
}

// Cancel pending approvals in the same saved snapshot as the owner's removal
// or demotion. Restoring that phone must never restore an old invitation.
func (i *Identity) cancelOwnerInvitesLocked(owner string) {
	now, _ := i.wallLocked()
	for id, v := range i.invites {
		if v.State == "pending" && v.ApprovedBy == owner {
			v.State = "cancelled"
			v.CancelledAtMs = now
			i.invites[id] = v
		}
	}
}

func (i *Identity) inviteViewLocked(v storedInvite, now int64, clock bool) protocol.AgentInvite {
	r := v.AgentInvite
	r.Box = bytes.Clone(r.Box)
	r.Scopes, r.Ops = slices.Clone(r.Scopes), slices.Clone(r.Ops)
	r.ClockUnavailable = !clock
	if r.State == "pending" && (clock && now >= r.ClaimUntilMs || !i.inviteOwnerLocked(v.ApprovedBy)) {
		r.State = "expired"
	}
	return r
}

func (i *Identity) AgentInvites() []protocol.AgentInvite {
	i.mu.Lock()
	defer i.mu.Unlock()
	now, ok := i.wallLocked()
	rows := make([]protocol.AgentInvite, 0, len(i.invites))
	for _, v := range i.invites {
		rows = append(rows, i.inviteViewLocked(v, now, ok))
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].CreatedAtMs > rows[b].CreatedAtMs })
	return rows
}

// CreateAgentInvite saves approval before returning a code can be shared.
// An exact retry returns the saved receipt, including after claim or cancel.
func (i *Identity) CreateAgentInvite(by Approver, m protocol.AgentInviteCreate) (protocol.AgentInvite, error) {
	home := i.homeID()
	i.saveMu.Lock()
	i.mu.Lock()
	row, changed, err := i.createAgentInviteLocked(by, m, home)
	i.mu.Unlock()
	i.saveMu.Unlock()
	if changed {
		i.changed()
	}
	return row, err
}

func (i *Identity) createAgentInviteLocked(by Approver, m protocol.AgentInviteCreate, home string) (protocol.AgentInvite, bool, error) {
	now, ok := i.wallLocked()
	if !ok {
		return protocol.AgentInvite{}, false, noClock()
	}
	id, err := base64.RawURLEncoding.DecodeString(m.ID)
	if err != nil || len(id) != 16 || base64.RawURLEncoding.EncodeToString(id) != m.ID || len(m.Hash) != sha256.Size || len(m.Label) > 128 {
		return protocol.AgentInvite{}, false, badField("invitation")
	}
	approvedBy := LANApprover
	if by.DeviceID != "" {
		key, meta := i.findLocked(by.DeviceID)
		if meta == nil || meta.role != apiauth.RoleOwner || meta.agent != nil {
			return protocol.AgentInvite{}, false, refuse(protocol.ErrScopeDenied, nil)
		}
		approvedBy = key
	}
	raw, err := protocol.InviteCreateBytes(protocol.SignedFor{BoxKey: i.static.Public(), DeviceID: by.DeviceID}, m)
	if err != nil {
		return protocol.AgentInvite{}, false, badField("order")
	}
	sum := sha256.Sum256(raw)
	if old, exists := i.invites[m.ID]; exists {
		if old.ApprovedBy != approvedBy || !bytes.Equal(old.RequestHash, sum[:]) {
			return protocol.AgentInvite{}, false, refuse(protocol.ErrConflict, nil)
		}
		return i.inviteViewLocked(old, now, true), false, nil
	}
	if home == "" || m.Home != home {
		return protocol.AgentInvite{}, false, badField("home")
	}
	if m.ClaimUntilMs <= now || m.ClaimUntilMs > now+protocol.AgentInviteClaimMaxMs || m.ClaimUntilMs > m.ExpiresAtMs {
		return protocol.AgentInvite{}, false, badField("claimUntilMs")
	}
	caps, err := checkGrant(apiauth.RoleAgent, m.Scopes, m.Ops, m.Caps, m.ExpiresAtMs, protocol.Principal{}, now)
	if err != nil {
		return protocol.AgentInvite{}, false, err
	}
	active := 0
	for _, v := range i.invites {
		if i.inviteViewLocked(v, now, true).State == "pending" {
			active++
		}
	}
	if active >= protocol.AgentInviteRowsMax {
		return protocol.AgentInvite{}, false, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "invitations", "max": protocol.AgentInviteRowsMax})
	}
	for _, v := range i.invites {
		if bytes.Equal(v.Hash, m.Hash) {
			return protocol.AgentInvite{}, false, badField("hash")
		}
	}
	approvedBy, err = i.approveLocked(by, m.AtMs, func(f protocol.SignedFor) ([]byte, error) { return protocol.InviteCreateBytes(f, m) }, m.Sig, now)
	if err != nil {
		return protocol.AgentInvite{}, false, err
	}
	v := storedInvite{AgentInvite: protocol.AgentInvite{Home: home, Box: i.static.Public(), ID: m.ID, Label: m.Label, Scopes: slices.Clone(m.Scopes), Ops: slices.Clone(m.Ops), Caps: caps,
		CreatedAtMs: now, ClaimUntilMs: m.ClaimUntilMs, ExpiresAtMs: m.ExpiresAtMs, State: "pending"}, Hash: bytes.Clone(m.Hash), RequestHash: sum[:], ApprovedBy: approvedBy}
	s := i.snapshotLocked()
	s.AgentInvites = append(s.AgentInvites, v)
	// Keep live claim windows and active agents, then the 32 newest old
	// receipts. A cancelled code cannot become new after a lost retry.
	sort.Slice(s.AgentInvites, func(a, b int) bool {
		left, right := s.AgentInvites[a], s.AgentInvites[b]
		at, bt := max(left.CreatedAtMs, left.ClaimedAtMs, left.CancelledAtMs), max(right.CreatedAtMs, right.ClaimedAtMs, right.CancelledAtMs)
		if at == bt {
			return left.ID < right.ID
		}
		return at > bt
	})
	finished := 0
	kept := s.AgentInvites[:0]
	for _, row := range s.AgentInvites {
		liveAgent := row.AgentKey != "" && i.authorised[row.AgentKey] != nil
		if now >= row.ClaimUntilMs && !liveAgent {
			finished++
			if finished > 32 {
				continue
			}
		}
		kept = append(kept, row)
	}
	s.AgentInvites = kept
	if len(kept) > 128 {
		if by.DeviceID != "" {
			delete(i.seenApprovals, sum)
		}
		return protocol.AgentInvite{}, false, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "invitationReceipts", "max": 128})
	}
	if err := i.persistLocked(s); err != nil {
		if by.DeviceID != "" {
			delete(i.seenApprovals, sum)
		}
		return protocol.AgentInvite{}, false, err
	}
	i.invites = map[string]storedInvite{}
	for _, row := range s.AgentInvites {
		i.invites[row.ID] = row
	}
	return i.inviteViewLocked(v, now, true), true, nil
}

func (i *Identity) CancelAgentInvite(id string) (protocol.AgentInvite, error) {
	i.saveMu.Lock()
	i.mu.Lock()
	v, exists := i.invites[id]
	if !exists {
		i.mu.Unlock()
		i.saveMu.Unlock()
		return protocol.AgentInvite{}, refuse(protocol.ErrNotFound, nil)
	}
	// Cancelling an already claimed code does not revoke its agent. The
	// owner sees that row and uses the ordinary Revoke operation for it.
	if v.State != "pending" {
		i.mu.Unlock()
		i.saveMu.Unlock()
		return v.AgentInvite, nil
	}
	v.State = "cancelled"
	v.CancelledAtMs, _ = i.wallLocked()
	s := i.snapshotLocked()
	for n := range s.AgentInvites {
		if s.AgentInvites[n].ID == id {
			s.AgentInvites[n] = v
		}
	}
	err := i.persistLocked(s)
	if err == nil {
		i.invites[id] = v
	}
	i.mu.Unlock()
	i.saveMu.Unlock()
	if err == nil {
		i.changed()
	}
	return v.AgentInvite, err
}

func (i *Identity) CancelAllAgentInvites() error {
	i.saveMu.Lock()
	i.mu.Lock()
	now, _ := i.wallLocked()
	s := i.snapshotLocked()
	changed := false
	for n := range s.AgentInvites {
		if s.AgentInvites[n].State == "pending" {
			s.AgentInvites[n].State = "cancelled"
			s.AgentInvites[n].CancelledAtMs = now
			changed = true
		}
	}
	var err error
	if changed {
		err = i.persistLocked(s)
		if err == nil {
			for _, v := range s.AgentInvites {
				i.invites[v.ID] = v
			}
		}
	}
	i.mu.Unlock()
	i.saveMu.Unlock()
	if changed && err == nil {
		i.changed()
	}
	return err
}

func (i *Identity) checkAgentInvite(appStatic, payload []byte) (Grant, func() (Grant, error), error) {
	c, err := protocol.ParseAgentInviteClaim(payload)
	if err != nil || len(appStatic) != 32 {
		return Grant{}, nil, ErrBadPairing
	}
	home := i.homeID()
	i.mu.Lock()
	id, meta, err := i.inviteClaimLocked(appStatic, c, home)
	if err != nil {
		i.mu.Unlock()
		return Grant{}, nil, err
	}
	key := base64.RawURLEncoding.EncodeToString(appStatic)
	g := i.grantLocked(key, meta)
	i.mu.Unlock()
	return g, func() (Grant, error) {
		home := i.homeID()
		i.saveMu.Lock()
		i.mu.Lock()
		// Verify the code, signed request, owner, time and current row again
		// after the recipient proves possession in the first transport frame.
		gotID, current, err := i.inviteClaimLocked(appStatic, c, home)
		if err != nil || gotID != id {
			i.mu.Unlock()
			i.saveMu.Unlock()
			return Grant{}, ErrBadPairing
		}
		v := i.invites[id]
		if v.State == "claimed" {
			g := i.grantLocked(key, current)
			i.mu.Unlock()
			i.saveMu.Unlock()
			return g, nil
		}
		offer, _ := protocol.Marshal(c.Offer)
		sum := sha256.Sum256(offer)
		now, _ := i.wallLocked()
		v.State, v.AgentID, v.AgentKey, v.ClaimedAtMs, v.Client, v.OfferHash = "claimed", deviceID(key), key, now, current.agent.Principal.Client, sum[:]
		s := i.snapshotLocked()
		for n := range s.AgentInvites {
			if s.AgentInvites[n].ID == id {
				s.AgentInvites[n] = v
			}
		}
		s.AuthorisedApps = append(s.AuthorisedApps, storedApp{Key: key, Role: current.role, Epoch: current.epoch, AddedAtMs: now, LastSeenMs: now, Agent: current.agent})
		if err := i.persistLocked(s); err != nil {
			i.mu.Unlock()
			i.saveMu.Unlock()
			return Grant{}, err
		}
		i.invites[id], i.authorised[key] = v, current
		g := i.grantLocked(key, current)
		row := i.agentRowLocked(key, current)
		notify := i.onInviteClaimed
		i.mu.Unlock()
		i.saveMu.Unlock()
		if notify != nil {
			notify(v.AgentInvite, row)
		}
		// No callback runs under the identity lock.
		i.changed()
		return g, nil
	}, nil
}

func (i *Identity) inviteClaimLocked(key []byte, c protocol.AgentInviteClaim, home string) (string, *appMeta, error) {
	now, ok := i.wallLocked()
	if !ok || home == "" {
		return "", nil, ErrBadPairing
	}
	hash := protocol.AgentInviteHash(home, c.Secret)
	var v storedInvite
	found := false
	for _, row := range i.invites {
		if bytes.Equal(hash, row.Hash) {
			v, found = row, true
			break
		}
	}
	if !found || v.Home != home || !i.inviteOwnerLocked(v.ApprovedBy) {
		return "", nil, ErrBadPairing
	}
	offer, err := protocol.OpenOffer(i.offerKeys, c.Offer, now)
	if err != nil || offer.Role != apiauth.RoleAgent || !bytes.Equal(key, offer.Key) || offer.Home != "" && offer.Home != home {
		return "", nil, ErrBadPairing
	}
	keyString := base64.RawURLEncoding.EncodeToString(key)
	if v.State == "claimed" {
		raw, _ := protocol.Marshal(c.Offer)
		sum := sha256.Sum256(raw)
		meta := i.authorised[keyString]
		if v.AgentKey != keyString || !bytes.Equal(v.OfferHash, sum[:]) || meta == nil || meta.agent == nil {
			return "", nil, ErrBadPairing
		}
		g, _, valid := i.agentGrantLocked(meta)
		if !valid || g.Expired || g.NoClock {
			return "", nil, ErrBadPairing
		}
		return v.ID, meta, nil
	}
	if v.State != "pending" || now >= v.ClaimUntilMs || now >= v.ExpiresAtMs || i.authorised[keyString] != nil {
		return "", nil, ErrBadPairing
	}
	if _, taken := i.findLocked(deviceID(keyString)); taken != nil || i.agentCountLocked() >= protocol.AgentRowsMax {
		return "", nil, ErrBadPairing
	}
	scopes, ops := []string{}, []string{}
	for _, s := range v.Scopes {
		if slices.Contains(offer.Scopes, s) {
			scopes = append(scopes, s)
		}
	}
	for _, op := range v.Ops {
		if slices.Contains(offer.Ops, op) && slices.Contains(scopes, protocol.RegistryAgentOps[op]) {
			ops = append(ops, op)
		}
	}
	caps := v.Caps
	caps.HoldS = min(caps.HoldS, offer.Caps.HoldS)
	caps.MaxW = min(caps.MaxW, offer.Caps.MaxW)
	perMin := offer.Caps.PerMin
	if perMin == 0 {
		perMin = protocol.PerMinDefault
	}
	caps.PerMin = min(caps.PerMin, perMin)
	if offer.TTLS == 0 || offer.TTLS > uint64(protocol.AgentMaxMs/1000) {
		return "", nil, ErrBadPairing
	}
	expires := min(v.ExpiresAtMs, now+int64(offer.TTLS)*1000)
	if !slices.Contains(scopes, protocol.ScopeDispatchHold) {
		caps.HoldS, caps.MaxW = 0, 0
	}
	principal := protocol.Principal{Client: offer.Client, ClientID: offer.ClientID, Ticket: offer.Ticket}
	caps, err = checkGrant(apiauth.RoleAgent, scopes, ops, caps, expires, principal, now)
	if err != nil {
		return "", nil, ErrBadPairing
	}
	meta := &appMeta{role: apiauth.RoleAgent, epoch: firstEpoch, addedAtMs: now, lastSeenMs: now, agent: &agentMeta{Scopes: scopes, Ops: ops, Caps: caps, ExpiresAtMs: expires, Principal: principal, ApprovedBy: v.ApprovedBy}}
	return v.ID, meta, nil
}

func (i *Identity) SetAgentInviteHome(fn func() (string, error)) {
	i.mu.Lock()
	i.prepareInviteHome = fn
	i.mu.Unlock()
}
func (i *Identity) PrepareAgentInviteHome() (string, error) {
	i.mu.Lock()
	fn := i.prepareInviteHome
	i.mu.Unlock()
	if fn == nil {
		return "", refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "cloud"})
	}
	return fn()
}
func (i *Identity) OnAgentInviteClaimed(fn func(protocol.AgentInvite, protocol.AgentRow)) {
	i.mu.Lock()
	i.onInviteClaimed = fn
	i.mu.Unlock()
}
