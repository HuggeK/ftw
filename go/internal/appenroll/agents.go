package appenroll

// Agent rows: grants an owner gives to an assistant, to Sourceful support for
// one ticket, or to the release check (docs/agents.md). An agent row is an
// enrolment like a phone's, keyed by its Noise static key, with a grant of
// its own: scopes, ops, caps, a wall-clock expiry and who approved it.
//
// This file decides every grant and checks every approval. A row is added or
// widened only with an owner phone's approval signature or from the box's
// own page, and each check of a live row asks the clock again.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

// LANApprover is ApprovedBy for a grant made on the box's own page.
const LANApprover = "lan"

// storedAgent is an agent row's grant on disk.
type storedAgent struct {
	Scopes      []string           `json:"scopes"`
	Ops         []string           `json:"ops"`
	Caps        protocol.Caps      `json:"caps"`
	ExpiresAtMs int64              `json:"expiresAtMs"`
	Principal   protocol.Principal `json:"principal"`
	// ApprovedBy is the full key of the approving phone, or LANApprover.
	ApprovedBy string `json:"approvedBy"`
}

// agentMeta is the same grant in memory.
type agentMeta = storedAgent

// AgentGrant is an agent row's grant as it stands at this moment.
type AgentGrant struct {
	Scopes      []string
	Ops         []string
	Caps        protocol.Caps
	ExpiresAtMs int64
	Principal   protocol.Principal
	// ApprovedBy is the approving phone's device id, or LANApprover.
	ApprovedBy string
	// Expired is true once the box's wall clock has passed ExpiresAtMs.
	// NoClock is true while the box cannot tell, and then the row may do
	// nothing.
	Expired bool
	NoClock bool
}

// Approver is who approves a grant: an owner phone over its session, by its
// device id, or the box's own page when DeviceID is empty.
type Approver struct {
	DeviceID string
}

// AtTheBoxPage approves from the box's own page on the home network.
var AtTheBoxPage = Approver{}

// SetClock gives enrolment the wall clock and whether NTP has set it. Until
// it is set, and whenever it answers false, no agent row may act.
func (i *Identity) SetClock(fn func() (time.Time, bool)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.wall = fn
}

// wallLocked is the wall clock in ms, and whether it can be trusted. The
// caller holds the mutex.
func (i *Identity) wallLocked() (int64, bool) {
	if i.wall == nil {
		return 0, false
	}
	now, ok := i.wall()
	if ok && i.sinceMs == 0 {
		// The first trusted time this process saw: approvals signed before
		// it are refused, since the replay memory starts here. A phone told
		// E_CLOCK_SKEW signs again with the box's time.
		i.sinceMs = now.UnixMilli()
	}
	return now.UnixMilli(), ok
}

func refuse(code string, args map[string]any) error { return protocol.Refuse(code, args) }

func badField(field string) error {
	return refuse(protocol.ErrUnknownOp, map[string]any{"field": field})
}

func noClock() error {
	return refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"})
}

// longest is the longest grant each agent role may have.
func longest(role string) int64 {
	switch role {
	case apiauth.RoleSupport:
		return protocol.SupportMaxMs
	case apiauth.RoleRelease:
		return protocol.ReleaseMaxMs
	default:
		return protocol.AgentMaxMs
	}
}

func sortedUnique(list []string) bool {
	return slices.IsSorted(list) && len(slices.Compact(slices.Clone(list))) == len(list)
}

// checkGrant refuses a grant the role, the registry or the box's limits do
// not allow. It returns the caps with the default rate filled in.
func checkGrant(role string, scopes, ops []string, caps protocol.Caps, expiresAtMs int64,
	principal protocol.Principal, nowMs int64) (protocol.Caps, error) {
	if !apiauth.AgentRole(role) {
		return caps, badField("role")
	}
	allowed := apiauth.RoleScopes[role]
	if len(scopes) == 0 || !sortedUnique(scopes) {
		return caps, badField("scopes")
	}
	for _, s := range scopes {
		if !slices.Contains(allowed, s) {
			return caps, refuse(protocol.ErrScopeDenied, map[string]any{"field": "scopes", "scope": s, "role": role})
		}
	}
	if !sortedUnique(ops) {
		return caps, badField("ops")
	}
	for _, op := range ops {
		need, ok := protocol.RegistryAgentOps[op]
		if !ok || !slices.Contains(scopes, need) {
			return caps, refuse(protocol.ErrOpNotGranted, map[string]any{"field": "ops", "op": op})
		}
	}
	if slices.Contains(scopes, protocol.ScopeDispatchHold) {
		if caps.HoldS < protocol.HoldSMin || caps.HoldS > protocol.HoldSMax {
			return caps, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "holdS", "min": protocol.HoldSMin, "max": protocol.HoldSMax})
		}
		if caps.MaxW < protocol.MaxWMin || caps.MaxW > protocol.MaxWMax {
			return caps, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "maxW", "min": protocol.MaxWMin, "max": protocol.MaxWMax})
		}
	} else if caps.HoldS != 0 || caps.MaxW != 0 {
		return caps, badField("caps")
	}
	if caps.PerMin == 0 {
		caps.PerMin = protocol.PerMinDefault
	}
	if caps.PerMin > protocol.PerMinMax {
		return caps, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "perMin", "max": protocol.PerMinMax})
	}
	if expiresAtMs <= nowMs {
		return caps, badField("expiresAtMs")
	}
	if most := nowMs + longest(role); expiresAtMs > most {
		return caps, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "expiresAtMs", "max": most})
	}
	for _, f := range []string{principal.Client, principal.ClientID, principal.Ticket} {
		if len(f) > protocol.PrincipalFieldMax {
			return caps, badField("principal")
		}
	}
	if role == apiauth.RoleSupport && principal.Ticket == "" {
		return caps, badField("principal.ticket")
	}
	if role == apiauth.RoleSupport && principal.ClientID != protocol.SupportClientID {
		// Support is Sourceful's tooling and nothing else, however the
		// grant reached the box.
		return caps, badField("principal.clientId")
	}
	return caps, nil
}

// approveLocked checks that an approval came from an owner and, for a
// phone, that its approval key signed exactly signed. The caller holds the
// mutex. It returns what ApprovedBy should say.
func (i *Identity) approveLocked(by Approver, atMs int64, signed func(protocol.SignedFor) ([]byte, error), sig []byte, nowMs int64) (string, error) {
	if by.DeviceID == "" {
		return LANApprover, nil
	}
	key, meta := i.findLocked(by.DeviceID)
	if meta == nil || meta.role != apiauth.RoleOwner || meta.agent != nil {
		return "", refuse(protocol.ErrScopeDenied, map[string]any{"needRole": apiauth.RoleOwner})
	}
	if len(meta.approvalKey) == 0 {
		return "", refuse(protocol.ErrNoApprovalKey, nil)
	}
	if err := i.verifyLocked(meta.approvalKey, by.DeviceID, atMs, signed, sig, nowMs); err != nil {
		return "", err
	}
	return key, nil
}

// verifyLocked checks one signature: fresh, never seen, and over the bytes
// this box would build. The caller holds the mutex.
func (i *Identity) verifyLocked(pub []byte, deviceID string, atMs int64, signed func(protocol.SignedFor) ([]byte, error), sig []byte, nowMs int64) error {
	if len(sig) != protocol.ApprovalSigBytes {
		return refuse(protocol.ErrBadSignature, nil)
	}
	// The memory of what it took holds only since this process started, so
	// an approval from before then is refused rather than taken twice.
	if atMs < nowMs-protocol.ApprovalSkewMs || atMs > nowMs+protocol.ApprovalSkewMs || atMs < i.sinceMs {
		return refuse(protocol.ErrClockSkew, map[string]any{"boxMs": nowMs})
	}
	msg, err := signed(protocol.SignedFor{BoxKey: i.static.Public(), DeviceID: deviceID})
	if err != nil {
		return badField("order")
	}
	if protocol.VerifyApproval(pub, msg, sig) != nil {
		return refuse(protocol.ErrBadSignature, nil)
	}
	// Keyed by the signed bytes, not the signature: (r, s) and (r, N-s)
	// both verify, and either one approves these bytes once.
	seen := sha256.Sum256(msg)
	for d, until := range i.seenApprovals {
		if until <= nowMs {
			delete(i.seenApprovals, d)
		}
	}
	if _, again := i.seenApprovals[seen]; again {
		return refuse(protocol.ErrBadSignature, map[string]any{"field": "replay"})
	}
	if i.seenApprovals == nil {
		i.seenApprovals = map[[32]byte]int64{}
	}
	i.seenApprovals[seen] = nowMs + protocol.ApprovalReplayMs
	return nil
}

func (i *Identity) agentCountLocked() int {
	n := 0
	for _, meta := range i.authorised {
		if meta.agent != nil {
			n++
		}
	}
	return n
}

// AddAgent stores a new agent row from an owner's approval.
func (i *Identity) AddAgent(by Approver, m protocol.AgentAdd) (protocol.AgentRow, error) {
	i.mu.Lock()
	nowMs, ok := i.wallLocked()
	if !ok {
		i.mu.Unlock()
		return protocol.AgentRow{}, noClock()
	}
	caps, err := checkGrant(m.Role, m.Scopes, m.Ops, m.Caps, m.ExpiresAtMs, m.Principal, nowMs)
	if err != nil {
		i.mu.Unlock()
		return protocol.AgentRow{}, err
	}
	if len(m.Key) != 32 {
		i.mu.Unlock()
		return protocol.AgentRow{}, badField("key")
	}
	key := base64.RawURLEncoding.EncodeToString(m.Key)
	if _, taken := i.findLocked(deviceID(key)); taken != nil || i.authorised[key] != nil {
		// A row's id is its key's first eight characters. One id, one row:
		// a revoke or a widen by id must never reach another.
		i.mu.Unlock()
		return protocol.AgentRow{}, badField("key")
	}
	if i.agentCountLocked() >= protocol.AgentRowsMax {
		i.mu.Unlock()
		return protocol.AgentRow{}, refuse(protocol.ErrCapExceeded, map[string]any{"cap": "agents", "max": protocol.AgentRowsMax})
	}
	approvedBy, err := i.approveLocked(by, m.AtMs, func(f protocol.SignedFor) ([]byte, error) {
		return protocol.AddBytes(f, m)
	}, m.Sig, nowMs)
	if err != nil {
		i.mu.Unlock()
		return protocol.AgentRow{}, err
	}
	meta := &appMeta{addedAtMs: nowMs, role: m.Role, epoch: firstEpoch, agent: &agentMeta{
		Scopes: slices.Clone(m.Scopes), Ops: slices.Clone(m.Ops), Caps: caps, ExpiresAtMs: m.ExpiresAtMs,
		Principal: m.Principal, ApprovedBy: approvedBy,
	}}
	if meta.agent.Ops == nil {
		meta.agent.Ops = []string{}
	}
	i.authorised[key] = meta
	row := i.agentRowLocked(key, meta)
	i.mu.Unlock()

	err = i.save()
	i.changed()
	return row, err
}

// WidenAgent replaces an agent row's rights from an owner's approval.
func (i *Identity) WidenAgent(by Approver, m protocol.AgentWiden) (protocol.AgentRow, error) {
	i.mu.Lock()
	nowMs, ok := i.wallLocked()
	if !ok {
		i.mu.Unlock()
		return protocol.AgentRow{}, noClock()
	}
	key, meta := i.findLocked(m.ID)
	if meta == nil || meta.agent == nil {
		i.mu.Unlock()
		return protocol.AgentRow{}, refuse(protocol.ErrNotFound, map[string]any{"id": m.ID})
	}
	if meta.epoch != m.Epoch {
		i.mu.Unlock()
		return protocol.AgentRow{}, refuse(protocol.ErrConflict, map[string]any{"expected": m.Epoch, "actual": meta.epoch})
	}
	caps, err := checkGrant(meta.role, m.Scopes, m.Ops, m.Caps, m.ExpiresAtMs, meta.agent.Principal, nowMs)
	if err != nil {
		i.mu.Unlock()
		return protocol.AgentRow{}, err
	}
	rowKey, _ := base64.RawURLEncoding.DecodeString(key)
	approvedBy, err := i.approveLocked(by, m.AtMs, func(f protocol.SignedFor) ([]byte, error) {
		return protocol.WidenBytes(f, m, rowKey)
	}, m.Sig, nowMs)
	if err != nil {
		i.mu.Unlock()
		return protocol.AgentRow{}, err
	}
	ops := slices.Clone(m.Ops)
	if ops == nil {
		ops = []string{}
	}
	meta.agent = &agentMeta{
		Scopes: slices.Clone(m.Scopes), Ops: ops, Caps: caps, ExpiresAtMs: m.ExpiresAtMs,
		Principal: meta.agent.Principal, ApprovedBy: approvedBy,
	}
	meta.epoch++
	row := i.agentRowLocked(key, meta)
	i.mu.Unlock()

	err = i.save()
	i.changed()
	return row, err
}

// SetApprovalKey registers an owner phone's approval key. The signature
// proves the phone holds the private half. Replacing a key needs PrevSig,
// the old key's signature over the same bytes; a phone that lost its key
// replaces it on the box page instead (ReplaceApprovalKey).
func (i *Identity) SetApprovalKey(deviceID string, m protocol.ApprovalKeySet) (int64, error) {
	if _, err := protocol.ParseApprovalKey(m.Pub); err != nil {
		return 0, badField("pub")
	}
	i.mu.Lock()
	nowMs, ok := i.wallLocked()
	if !ok {
		i.mu.Unlock()
		return 0, noClock()
	}
	_, meta := i.findLocked(deviceID)
	if meta == nil || meta.role != apiauth.RoleOwner || meta.agent != nil {
		i.mu.Unlock()
		return 0, refuse(protocol.ErrScopeDenied, map[string]any{"needRole": apiauth.RoleOwner})
	}
	bytesFor := func(f protocol.SignedFor) ([]byte, error) { return protocol.KeySetBytes(f, m) }
	if len(meta.approvalKey) > 0 {
		msg, err := bytesFor(protocol.SignedFor{BoxKey: i.static.Public(), DeviceID: deviceID})
		if err != nil || protocol.VerifyApproval(meta.approvalKey, msg, m.PrevSig) != nil {
			i.mu.Unlock()
			return 0, refuse(protocol.ErrBadSignature, map[string]any{"field": "prevSig"})
		}
	}
	if err := i.verifyLocked(m.Pub, deviceID, m.AtMs, bytesFor, m.Sig, nowMs); err != nil {
		i.mu.Unlock()
		return 0, err
	}
	meta.approvalKey = slices.Clone(m.Pub)
	meta.approvalKeySetMs = nowMs
	i.mu.Unlock()
	return nowMs, i.save()
}

// ReplaceApprovalKey sets an owner phone's approval key from the box page,
// where the house password stands in for the old key: for a phone whose key
// a new Face ID enrolment voided.
func (i *Identity) ReplaceApprovalKey(deviceID string, pub []byte) (int64, error) {
	if _, err := protocol.ParseApprovalKey(pub); err != nil {
		return 0, badField("pub")
	}
	i.mu.Lock()
	nowMs, ok := i.wallLocked()
	_, meta := i.findLocked(deviceID)
	if meta == nil || meta.role != apiauth.RoleOwner || meta.agent != nil {
		i.mu.Unlock()
		return 0, refuse(protocol.ErrNotFound, map[string]any{"deviceId": deviceID})
	}
	if !ok {
		nowMs = 0
	}
	meta.approvalKey = slices.Clone(pub)
	meta.approvalKeySetMs = nowMs
	i.mu.Unlock()
	return nowMs, i.save()
}

// CheckConfirm checks an owner phone's signed approval of a pending request:
// the request id, the agent that asked, the op and its args.
func (i *Identity) CheckConfirm(deviceID string, m protocol.AgentConfirm, req protocol.Request) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	nowMs, ok := i.wallLocked()
	if !ok {
		return noClock()
	}
	_, err := i.approveLocked(Approver{DeviceID: deviceID}, m.AtMs, func(f protocol.SignedFor) ([]byte, error) {
		return protocol.ConfirmBytes(f, m, req)
	}, m.Sig, nowMs)
	return err
}

// ApprovalKeys lists the owner phones that can approve, by device id.
func (i *Identity) ApprovalKeys() []protocol.ApprovalKeyInfo {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []protocol.ApprovalKeyInfo
	for key, meta := range i.authorised {
		if len(meta.approvalKey) > 0 {
			out = append(out, protocol.ApprovalKeyInfo{DeviceID: deviceID(key), SetAtMs: meta.approvalKeySetMs})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].DeviceID < out[b].DeviceID })
	return out
}

// Agents lists every agent row, sorted by id.
func (i *Identity) Agents() []protocol.AgentRow {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []protocol.AgentRow
	for key, meta := range i.authorised {
		if meta.agent != nil {
			out = append(out, i.agentRowLocked(key, meta))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Agent is one agent row by id.
func (i *Identity) Agent(id string) (protocol.AgentRow, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	key, meta := i.findLocked(id)
	if meta == nil || meta.agent == nil {
		return protocol.AgentRow{}, false
	}
	return i.agentRowLocked(key, meta), true
}

func (i *Identity) agentRowLocked(key string, meta *appMeta) protocol.AgentRow {
	raw, _ := base64.RawURLEncoding.DecodeString(key)
	return protocol.AgentRow{
		ID: deviceID(key), Key: raw, Role: meta.role,
		Scopes: slices.Clone(meta.agent.Scopes), Ops: slices.Clone(meta.agent.Ops), Caps: meta.agent.Caps,
		ExpiresAtMs: meta.agent.ExpiresAtMs, Principal: meta.agent.Principal,
		ApprovedBy: i.approverNameLocked(meta.agent.ApprovedBy),
		AddedAtMs:  meta.addedAtMs, LastSeenMs: meta.lastSeenMs, Epoch: meta.epoch,
	}
}

// approverNameLocked is ApprovedBy as the wire says it: app:<id> or lan.
func (i *Identity) approverNameLocked(approvedBy string) string {
	if approvedBy == LANApprover {
		return LANApprover
	}
	return apiauth.KindApp + ":" + deviceID(approvedBy)
}

// agentGrantLocked is an agent row's grant now, and the role its approver
// holds now. ok is false when the approving phone is gone, which a revoke
// never leaves behind. The caller holds the mutex.
func (i *Identity) agentGrantLocked(meta *appMeta) (AgentGrant, string, bool) {
	approverRole := apiauth.RoleOwner
	approvedBy := LANApprover
	if meta.agent.ApprovedBy != LANApprover {
		phone := i.authorised[meta.agent.ApprovedBy]
		if phone == nil {
			return AgentGrant{}, "", false
		}
		approverRole = phone.role
		approvedBy = deviceID(meta.agent.ApprovedBy)
	}
	g := AgentGrant{
		Scopes: slices.Clone(meta.agent.Scopes), Ops: slices.Clone(meta.agent.Ops), Caps: meta.agent.Caps,
		ExpiresAtMs: meta.agent.ExpiresAtMs, Principal: meta.agent.Principal, ApprovedBy: approvedBy,
	}
	nowMs, ok := i.wallLocked()
	switch {
	case !ok:
		g.NoClock = true
	case nowMs >= meta.agent.ExpiresAtMs:
		g.Expired = true
	}
	return g, approverRole, true
}

// ExpireAgents removes every agent row the clock says has run out, and
// returns what it removed so the caller can end their sessions. With no
// clock it removes nothing; those rows already may do nothing.
func (i *Identity) ExpireAgents() []Removed {
	i.mu.Lock()
	nowMs, ok := i.wallLocked()
	if !ok {
		i.mu.Unlock()
		return nil
	}
	var gone []string
	for key, meta := range i.authorised {
		if meta.agent != nil && nowMs >= meta.agent.ExpiresAtMs {
			gone = append(gone, key)
		}
	}
	for _, key := range gone {
		delete(i.authorised, key)
	}
	i.mu.Unlock()
	if len(gone) == 0 {
		return nil
	}
	sort.Strings(gone)
	_ = i.save()
	i.changed()
	return removedKeys(gone)
}

func removedKeys(keys []string) []Removed {
	out := make([]Removed, 0, len(keys))
	for _, key := range keys {
		raw, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			continue
		}
		out = append(out, Removed{ID: deviceID(key), Key: raw})
	}
	return out
}

// ErrAgentRow is a phone's operation aimed at an agent row, or the other
// way round.
var ErrAgentRow = errors.New("appenroll: that is an agent row")

// DeriveKey is a key for label, derived from the box's static secret: the
// same for the life of the box, and useless for finding the secret.
func (i *Identity) DeriveKey(label string) []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	mac := hmac.New(sha256.New, i.staticSecret)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}
