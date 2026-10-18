package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

func TestLANSessionStorageKeepsOnlyDigestAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal("open database failed")
	}
	passwordVersion := hex.EncodeToString(make([]byte, 32))
	if err := s.SaveConfig("lan_auth_password", passwordVersion); err != nil {
		t.Fatal("save password version failed")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal("make opaque token failed")
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	now := time.Now().UnixMilli()
	expires := now + (30 * 24 * time.Hour).Milliseconds()
	if err := s.SaveLANSession(digest, expires, now, passwordVersion); err != nil {
		t.Fatal("save session failed")
	}
	var rawCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM lan_auth_sessions WHERE token_digest=? OR password_binding=?`, token, token).Scan(&rawCount); err != nil || rawCount != 0 {
		t.Fatal("raw token must not be stored")
	}
	if err := s.Close(); err != nil {
		t.Fatal("close database failed")
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal("reopen database failed")
	}
	defer s.Close()
	loaded, err := s.LoadLANSessions(now)
	if err != nil || len(loaded) != 1 || loaded[0].Digest != digest || loaded[0].ExpiresMS != expires {
		t.Fatal("saved session changed across reopen")
	}
	loaded, err = s.LoadLANSessions(expires)
	if err != nil || len(loaded) != 0 {
		t.Fatal("expired session must not load")
	}
	// Model a crash after the password commits but before session cleanup.
	if err := s.SaveConfig("lan_auth_password", passwordVersion+"-changed"); err != nil {
		t.Fatal("change password version failed")
	}
	loaded, err = s.LoadLANSessions(now)
	if err != nil || len(loaded) != 0 {
		t.Fatal("old-password session survived an interrupted revocation")
	}
	if err := s.SaveLANSession(digest, expires, now, passwordVersion); err == nil {
		t.Fatal("old-password login raced through a password change")
	}
}
