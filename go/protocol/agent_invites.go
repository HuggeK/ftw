package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

const (
	MsgAgentInviteCreate          = "agent.invite.create"
	MsgAgentInviteList            = "agent.invite.list"
	MsgAgentInviteCancel          = "agent.invite.cancel"
	AgentInviteSecretBytes        = 32
	AgentInviteRowsMax            = 16
	AgentInviteClaimMaxMs   int64 = 24 * 60 * 60 * 1000
	AgentInviteHandshakeMax       = 8192
)

// AgentInviteCreate approves one future agent. The owner draws Secret locally
// and sends only its verifier. ID makes a lost create reply safe to retry.
type AgentInviteCreate struct {
	Home         string   `cbor:"home" json:"home"`
	ID           string   `cbor:"id" json:"id"`
	Hash         []byte   `cbor:"hash" json:"hash"`
	Label        string   `cbor:"label" json:"label"`
	Scopes       []string `cbor:"scopes" json:"scopes"`
	Ops          []string `cbor:"ops" json:"ops"`
	Caps         Caps     `cbor:"caps" json:"caps"`
	ClaimUntilMs int64    `cbor:"claimUntilMs" json:"claimUntilMs"`
	ExpiresAtMs  int64    `cbor:"expiresAtMs" json:"expiresAtMs"`
	AtMs         int64    `cbor:"atMs" json:"atMs"`
	Sig          []byte   `cbor:"sig" json:"sig"`
}

type AgentInviteCancel struct {
	ID string `cbor:"id" json:"id"`
}

// AgentInvite contains no secret or verifier. A claimed invitation points to
// the ordinary agent row; revoking that row never revives its invitation.
type AgentInvite struct {
	CancelledAtMs    int64    `cbor:"cancelledAtMs,omitempty" json:"cancelledAtMs,omitempty"`
	ClockUnavailable bool     `cbor:"clockUnavailable,omitempty" json:"clockUnavailable,omitempty"`
	Home             string   `cbor:"home" json:"home"`
	Box              []byte   `cbor:"box" json:"box"`
	ID               string   `cbor:"id" json:"id"`
	Label            string   `cbor:"label" json:"label"`
	Scopes           []string `cbor:"scopes" json:"scopes"`
	Ops              []string `cbor:"ops" json:"ops"`
	Caps             Caps     `cbor:"caps" json:"caps"`
	CreatedAtMs      int64    `cbor:"createdAtMs" json:"createdAtMs"`
	ClaimUntilMs     int64    `cbor:"claimUntilMs" json:"claimUntilMs"`
	ExpiresAtMs      int64    `cbor:"expiresAtMs" json:"expiresAtMs"`
	State            string   `cbor:"state" json:"state"`
	AgentID          string   `cbor:"agentId,omitempty" json:"agentId,omitempty"`
	ClaimedAtMs      int64    `cbor:"claimedAtMs,omitempty" json:"claimedAtMs,omitempty"`
	Client           string   `cbor:"client,omitempty" json:"client,omitempty"`
}

func InviteCreateBytes(f SignedFor, m AgentInviteCreate) ([]byte, error) {
	return ApprovalBytes(MsgAgentInviteCreate, f, m.AtMs, m.Home, m.ID, m.Hash, m.Label,
		emptyIfNil(m.Scopes), emptyIfNil(m.Ops), capsArray(m.Caps), m.ClaimUntilMs, m.ExpiresAtMs)
}

// AgentInviteCode is copied to the person receiving access. It contains a
// 256-bit bearer secret; never put it in logs, a query string or a roster.
type AgentInviteCode struct {
	V      uint64 `cbor:"v"`
	Home   string `cbor:"home"`
	Box    []byte `cbor:"box"`
	Secret []byte `cbor:"secret"`
}

var ErrAgentInvite = errors.New("protocol: invalid agent invitation")

func (c AgentInviteCode) valid() bool {
	if c.V != 1 || len(c.Box) != 32 || len(c.Secret) != AgentInviteSecretBytes || len(c.Home) != 26 {
		return false
	}
	for _, b := range c.Home {
		if !(b >= 'a' && b <= 'z' || b >= '2' && b <= '7') {
			return false
		}
	}
	return true
}

func (c AgentInviteCode) Encode() (string, error) {
	if !c.valid() {
		return "", ErrAgentInvite
	}
	b, err := encMode.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b), err
}

func ParseAgentInviteCode(raw string) (AgentInviteCode, error) {
	var c AgentInviteCode
	if len(raw) > 512 {
		return c, ErrAgentInvite
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || strictDec.Unmarshal(b, &c) != nil || !c.valid() {
		return AgentInviteCode{}, ErrAgentInvite
	}
	return c, nil
}

func AgentInviteHash(home string, secret []byte) []byte {
	h := sha256.New()
	h.Write([]byte("ftw.agent.invite.v1\x00"))
	h.Write([]byte(home))
	h.Write([]byte{0})
	h.Write(secret)
	return h.Sum(nil)
}

var agentInvitePrefix = []byte("ftw.agent.invite.v1\x00")

// AgentInviteClaim is encrypted inside Noise message 1. The signed offer
// binds the recipient's own pending key to the OAuth request.
type AgentInviteClaim struct {
	Secret []byte      `cbor:"secret"`
	Offer  SignedOffer `cbor:"offer"`
}

func (c AgentInviteClaim) Payload() ([]byte, error) {
	if len(c.Secret) != AgentInviteSecretBytes {
		return nil, ErrAgentInvite
	}
	b, err := encMode.Marshal(c)
	if err != nil || len(b)+len(agentInvitePrefix) > AgentInviteHandshakeMax {
		return nil, ErrAgentInvite
	}
	return append(bytes.Clone(agentInvitePrefix), b...), nil
}

func IsAgentInvitePayload(b []byte) bool { return bytes.HasPrefix(b, agentInvitePrefix) }

func ParseAgentInviteClaim(b []byte) (AgentInviteClaim, error) {
	var c AgentInviteClaim
	if !IsAgentInvitePayload(b) || len(b) > AgentInviteHandshakeMax ||
		strictDec.Unmarshal(b[len(agentInvitePrefix):], &c) != nil || len(c.Secret) != AgentInviteSecretBytes {
		return c, ErrAgentInvite
	}
	return c, nil
}
