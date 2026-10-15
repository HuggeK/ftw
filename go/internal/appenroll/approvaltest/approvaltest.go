// Package approvaltest signs approvals the way an owner phone's Secure
// Enclave key does, for tests of the box's side.
package approvaltest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"

	"github.com/srcfl/ftw/go/protocol"
)

// Signer is one phone's approval key.
type Signer struct {
	key *ecdsa.PrivateKey
}

// New draws a key.
func New() *Signer {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Signer{key: key}
}

// Pub is the key's public half, SEC1 uncompressed.
func (s *Signer) Pub() []byte {
	return elliptic.Marshal(elliptic.P256(), s.key.X, s.key.Y) //nolint:staticcheck // SEC1 uncompressed is the wire form.
}

// Sign signs msg, r ‖ s, over its SHA-256.
func (s *Signer) Sign(msg []byte) []byte {
	digest := sha256.Sum256(msg)
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		panic(err)
	}
	out := make([]byte, protocol.ApprovalSigBytes)
	r.FillBytes(out[:32])
	ss.FillBytes(out[32:])
	return out
}

// Replace is a signed approval.key.set that replaces old's key with this
// one.
func (s *Signer) Replace(f protocol.SignedFor, atMs int64, old *Signer) protocol.ApprovalKeySet {
	m := s.KeySet(f, atMs)
	raw, err := protocol.KeySetBytes(f, m)
	if err != nil {
		panic(err)
	}
	m.PrevSig = old.Sign(raw)
	return m
}

// KeySet is a signed approval.key.set.
func (s *Signer) KeySet(f protocol.SignedFor, atMs int64) protocol.ApprovalKeySet {
	m := protocol.ApprovalKeySet{Pub: s.Pub(), AtMs: atMs}
	raw, err := protocol.KeySetBytes(f, m)
	if err != nil {
		panic(err)
	}
	m.Sig = s.Sign(raw)
	return m
}

// Add signs m.
func (s *Signer) Add(f protocol.SignedFor, m protocol.AgentAdd) protocol.AgentAdd {
	raw, err := protocol.AddBytes(f, m)
	if err != nil {
		panic(err)
	}
	m.Sig = s.Sign(raw)
	return m
}

// Widen signs m for the row with key.
func (s *Signer) Widen(f protocol.SignedFor, m protocol.AgentWiden, key []byte) protocol.AgentWiden {
	raw, err := protocol.WidenBytes(f, m, key)
	if err != nil {
		panic(err)
	}
	m.Sig = s.Sign(raw)
	return m
}

// Confirm signs m for the request req.
func (s *Signer) Confirm(f protocol.SignedFor, m protocol.AgentConfirm, req protocol.Request) protocol.AgentConfirm {
	raw, err := protocol.ConfirmBytes(f, m, req)
	if err != nil {
		panic(err)
	}
	m.Sig = s.Sign(raw)
	return m
}
