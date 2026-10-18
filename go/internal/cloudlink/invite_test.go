package cloudlink

import (
	"crypto/ecdsa"
	"path/filepath"
	"sync"
	"testing"
)

type blockedInviteRoster struct {
	fakeRoster
	once             sync.Once
	entered, release chan struct{}
}

func (r *blockedInviteRoster) Snapshot(string) RosterSnapshot {
	r.once.Do(func() { close(r.entered); <-r.release })
	return RosterSnapshot{}
}

func TestInvitePreparationCannotBeEndedByAnOlderIdleCheck(t *testing.T) {
	svc := newFakeService(t)
	r := &blockedInviteRoster{entered: make(chan struct{}), release: make(chan struct{})}
	link, err := New(Options{Site: &fakeSite{}, Roster: r, KV: &memKV{}, KeyPath: filepath.Join(t.TempDir(), "cloud.key"), Tunnel: &fakeTunnel{}, ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey}, Endpoint: svc.endpoint(), RootCAs: svc.roots()})
	if err != nil {
		t.Fatal(err)
	}
	// Make an idle link without starting an invitation; the controlled
	// snapshot stops its final cleanup between the two link-lock phases.
	link.mu.Lock()
	err = link.linkLocked()
	link.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { link.endIfIdle(); close(done) }()
	<-r.entered
	home, err := link.PrepareAgentInviteHome()
	if err != nil || home == "" {
		close(r.release)
		t.Fatalf("prepare: %s %v", home, err)
	}
	close(r.release)
	<-done
	if link.Home() != home {
		t.Fatal("old idle check ended the newly prepared home")
	}
}
