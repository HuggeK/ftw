package appenroll

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/protocol"
)

const inviteHome = "abcdefghijklmnopqrstuvwx23"

type inviteFixture struct {
	b        *agentBox
	create   protocol.AgentInviteCreate
	secret   []byte
	offerKey *approvaltest.Signer
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	b := newAgentBox(t)
	b.id.SetHome(func() string { return inviteHome })
	f := &inviteFixture{b: b, secret: bytes.Repeat([]byte{0x91}, 32), offerKey: approvaltest.New()}
	b.id.SetAgentOfferKeys(map[string][]byte{"test": f.offerKey.Pub()})
	f.create = protocol.AgentInviteCreate{Home: inviteHome, ID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, 16)), Hash: protocol.AgentInviteHash(inviteHome, f.secret), Label: "Hannes", Scopes: []string{protocol.ScopeDiagRun, protocol.ScopeLiveRead}, ClaimUntilMs: b.ms() + 60_000, ExpiresAtMs: b.ms() + 86_400_000, AtMs: b.ms()}
	raw, _ := protocol.InviteCreateBytes(b.signedFor(b.owner.DeviceID), f.create)
	f.create.Sig = b.signer.Sign(raw)
	return f
}

func (f *inviteFixture) make(t *testing.T) protocol.AgentInvite {
	t.Helper()
	v, err := f.b.id.CreateAgentInvite(Approver{DeviceID: f.b.owner.DeviceID}, f.create)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *inviteFixture) claim(t *testing.T, key []byte) []byte {
	t.Helper()
	o := protocol.GrantOffer{V: 1, Key: key, Role: apiauth.RoleAgent, Client: "ChatGPT", ClientID: "https://client.example/metadata", Scopes: []string{protocol.ScopeDiagRun, protocol.ScopeLiveRead, protocol.ScopeSetupWrite}, TTLS: uint64(protocol.AgentMaxMs / 1000), ExpMs: f.b.ms() + 60_000}
	raw, _ := protocol.Marshal(o)
	signed, _ := protocol.OfferBytes(raw)
	p, err := (protocol.AgentInviteClaim{Secret: f.secret, Offer: protocol.SignedOffer{O: raw, Kid: "test", Sig: f.offerKey.Sign(signed)}}).Payload()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentInvitationNeedsOwnerAndIsSavedBeforePublication(t *testing.T) {
	f := newInviteFixture(t)
	b := f.b
	unsigned := f.create
	unsigned.Sig = nil
	if _, err := b.id.CreateAgentInvite(Approver{DeviceID: b.owner.DeviceID}, unsigned); refusalCode(err) != protocol.ErrBadSignature {
		t.Fatalf("unsigned: %v", err)
	}
	original := b.id.path
	b.id.path = filepath.Join(t.TempDir(), "missing", "identity.json")
	if _, err := b.id.CreateAgentInvite(Approver{DeviceID: b.owner.DeviceID}, f.create); err == nil {
		t.Fatal("create succeeded without a saved receipt")
	}
	if len(b.id.AgentInvites()) != 0 {
		t.Fatal("failed create published in memory")
	}
	b.id.path = original
	v := f.make(t) // Same signature, including after failed persistence.
	if v.Home != inviteHome || !bytes.Equal(v.Box, b.id.StaticKey().Public()) {
		t.Fatalf("wrong code binding: %+v", v)
	}
	raw, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(base64.RawURLEncoding.EncodeToString(f.secret))) {
		t.Fatal("secret saved on disk")
	}
	if _, err := b.id.CreateAgentInvite(Approver{DeviceID: b.owner.DeviceID}, f.create); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	changed := f.create
	changed.Label = "Somebody else"
	if _, err := b.id.CreateAgentInvite(Approver{DeviceID: b.owner.DeviceID}, changed); refusalCode(err) != protocol.ErrConflict {
		t.Fatalf("id overwritten: %v", err)
	}
}

func TestAgentInvitationConsumesOnlyAtProofAndCannotReviveARevokedKey(t *testing.T) {
	f := newInviteFixture(t)
	f.make(t)
	key := appKey(t)
	p := f.claim(t, key)
	_, commit, err := f.b.id.CheckTunnel(key, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.b.id.Agents()) != 0 || f.b.id.AgentInvites()[0].State != "pending" {
		t.Fatal("handshake alone spent invitation")
	}
	g, err := commit()
	if err != nil {
		t.Fatal(err)
	}
	if g.Role != apiauth.RoleAgent || g.Agent == nil || slicesContain(g.Agent.Scopes, protocol.ScopeSetupWrite) {
		t.Fatalf("claim widened grant: %+v", g)
	}
	row := f.b.id.AgentInvites()[0]
	if row.State != "claimed" || row.AgentID != g.DeviceID {
		t.Fatalf("missing receipt: %+v", row)
	}
	_, retry, err := f.b.id.CheckTunnel(key, p)
	if err != nil {
		t.Fatalf("lost reply retry: %v", err)
	}
	if _, err := retry(); err != nil {
		t.Fatal(err)
	}
	other := appKey(t)
	if _, _, err := f.b.id.CheckTunnel(other, f.claim(t, other)); err == nil {
		t.Fatal("second key reused code")
	}
	if _, err := f.b.id.Revoke(g.DeviceID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.b.id.CheckTunnel(key, p); err == nil {
		t.Fatal("claimed code revived revoked agent")
	}
	if _, _, err := f.b.id.Check(key, p); err == nil {
		t.Fatal("invitation accepted outside Cloud tunnel")
	}
}

func slicesContain(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestAgentInvitationFinalCommitRechecksCancelOwnerClockAndOffer(t *testing.T) {
	for _, change := range []string{"cancel", "owner", "clock", "offer", "home"} {
		t.Run(change, func(t *testing.T) {
			f := newInviteFixture(t)
			f.make(t)
			key := appKey(t)
			p := f.claim(t, key)
			_, commit, err := f.b.id.CheckTunnel(key, p)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "cancel":
				_, err = f.b.id.CancelAgentInvite(f.create.ID)
			case "owner":
				_ = pair(t, f.b.id, apiauth.RoleOwner)
				_, err = f.b.id.Revoke(f.b.owner.DeviceID, OverASession)
			case "clock":
				f.b.synced = false
			case "offer":
				f.b.now = f.b.now.Add(2 * time.Minute)
			case "home":
				f.b.id.SetHome(func() string { return "zyxwvutsrqponmlkjihgfedc23" })
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := commit(); err == nil {
				t.Fatal("stale claim committed")
			}
			if len(f.b.id.Agents()) != 0 {
				t.Fatal("refused claim left an agent")
			}
		})
	}
}

func TestAgentInvitationAtomicClaimAndRestart(t *testing.T) {
	f := newInviteFixture(t)
	f.make(t)
	var commits []func() (Grant, error)
	for range 2 {
		key := appKey(t)
		_, commit, err := f.b.id.CheckTunnel(key, f.claim(t, key))
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, commit)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, commit := range commits {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := commit(); results <- err }()
	}
	wg.Wait()
	close(results)
	good := 0
	for err := range results {
		if err == nil {
			good++
		}
	}
	if good != 1 || len(f.b.id.Agents()) != 1 {
		t.Fatalf("claim winners=%d", good)
	}
	_, err := f.b.id.CancelAgentInvite(f.create.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.b.id.AgentInvites()[0].State != "claimed" {
		t.Fatal("cancel hid already granted access")
	}
	keyPath := filepath.Join(filepath.Dir(f.b.id.path), "nova.key")
	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	again.SetClock(func() (time.Time, bool) { return f.b.now, true })
	if len(again.Agents()) != 1 || again.AgentInvites()[0].State != "claimed" {
		t.Fatal("restart lost atomic claim")
	}
}

func TestAgentInvitationRetriesAfterExpiryAndDoesNotCallHomeUnderLock(t *testing.T) {
	f := newInviteFixture(t)
	f.make(t)
	f.b.id.SetHome(func() string { _ = f.b.id.StaticKey(); return inviteHome })
	key := appKey(t)
	if _, _, err := f.b.id.CheckTunnel(key, f.claim(t, key)); err != nil {
		t.Fatal(err)
	}
	f.b.synced = false
	v := f.b.id.AgentInvites()[0]
	if v.State != "pending" || !v.ClockUnavailable {
		t.Fatalf("clock gap pretended expiry: %+v", v)
	}
	f.b.synced = true
	f.b.now = f.b.now.Add(time.Hour)
	v, err := f.b.id.CreateAgentInvite(Approver{DeviceID: f.b.owner.DeviceID}, f.create)
	if err != nil || v.State != "expired" {
		t.Fatalf("expired exact retry: %+v %v", v, err)
	}
}

func TestCancelledInvitationCannotReturnAfterReceiptPruning(t *testing.T) {
	f := newInviteFixture(t)
	f.make(t)
	if _, err := f.b.id.CancelAgentInvite(f.create.ID); err != nil {
		t.Fatal(err)
	}
	// More than the finished-receipt budget, all within the original code's
	// claim window. An exact retry must still return its cancellation.
	for n := 0; n < 40; n++ {
		m := f.create
		m.ID = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(n)}, 16))
		m.Hash = protocol.AgentInviteHash(inviteHome, bytes.Repeat([]byte{byte(n)}, 32))
		m.Sig = nil
		if _, err := f.b.id.CreateAgentInvite(AtTheBoxPage, m); err != nil {
			t.Fatal(err)
		}
		if _, err := f.b.id.CancelAgentInvite(m.ID); err != nil {
			t.Fatal(err)
		}
	}
	row, err := f.b.id.CreateAgentInvite(Approver{DeviceID: f.b.owner.DeviceID}, f.create)
	if err != nil || row.State != "cancelled" {
		t.Fatalf("cancelled code returned: %+v %v", row, err)
	}
	key := appKey(t)
	if _, _, err := f.b.id.CheckTunnel(key, f.claim(t, key)); err == nil {
		t.Fatal("cancelled code admitted a recipient")
	}
}

func TestOwnerRestorationNeverRevivesAnInvitation(t *testing.T) {
	for _, change := range []string{"demote", "revoke"} {
		t.Run(change, func(t *testing.T) {
			f := newInviteFixture(t)
			f.make(t)
			id := f.b.id
			pair(t, id, apiauth.RoleOwner)
			id.mu.Lock()
			fullKey, _ := id.findLocked(f.b.owner.DeviceID)
			id.mu.Unlock()
			ownerKey, err := base64.RawURLEncoding.DecodeString(fullKey)
			if err != nil {
				t.Fatal(err)
			}
			recipient := appKey(t)
			payload := f.claim(t, recipient)
			_, pendingCommit, err := id.CheckTunnel(recipient, payload)
			if err != nil {
				t.Fatal(err)
			}
			if change == "demote" {
				if err := id.SetRole(f.b.owner.DeviceID, apiauth.RoleViewer); err != nil {
					t.Fatal(err)
				}
				if err := id.SetRole(f.b.owner.DeviceID, apiauth.RoleOwner); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := id.Revoke(f.b.owner.DeviceID, OverASession); err != nil {
					t.Fatal(err)
				}
				code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := id.Authorise(ownerKey, code); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pendingCommit(); err == nil {
				t.Fatal("old proof committed after owner restoration")
			}
			again, err := LoadOrCreate(filepath.Join(filepath.Dir(id.path), "nova.key"))
			if err != nil {
				t.Fatal(err)
			}
			again.SetClock(func() (time.Time, bool) { return f.b.now, true })
			again.SetHome(func() string { return inviteHome })
			again.SetAgentOfferKeys(map[string][]byte{"test": f.offerKey.Pub()})
			for _, identity := range []*Identity{id, again} {
				row := identity.AgentInvites()[0]
				if row.State != "cancelled" || row.CancelledAtMs != f.b.ms() {
					t.Fatalf("restored invitation: %+v", row)
				}
				if _, _, err := identity.CheckTunnel(recipient, payload); err == nil {
					t.Fatal("owner restoration revived invitation")
				}
				if len(identity.Agents()) != 0 {
					t.Fatal("cancelled invitation granted access")
				}
			}
		})
	}
}
