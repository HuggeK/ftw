package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newLANSessionTestValue(t *testing.T) string {
	t.Helper()
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal("generate test value")
	}
	return hex.EncodeToString(value[:])
}

func lanSessionTestErrorCode(err error) int {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return 0
}

func TestLANSessionWritesWaitForWALWriter(t *testing.T) {
	for _, operation := range []string{"save_first_table", "save_existing_table", "save_password_changed", "delete_one", "delete_all"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal("open test store")
			}
			defer store.Close()
			password := newLANSessionTestValue(t)
			replacementPassword := newLANSessionTestValue(t)
			digest := newLANSessionTestValue(t)
			otherDigest := newLANSessionTestValue(t)
			newDigest := newLANSessionTestValue(t)
			now := time.Now().UnixMilli()
			expires := now + time.Hour.Milliseconds()
			if err := store.SaveConfig("lan_auth_password", password); err != nil {
				t.Fatal("save test password")
			}
			if operation != "save_first_table" {
				if err := store.SaveLANSession(digest, expires, now, password); err != nil {
					t.Fatal("prepare session table")
				}
				if err := store.SaveLANSession(otherDigest, expires, now, password); err != nil {
					t.Fatal("prepare retained session")
				}
			}
			writer, err := openRaw(path)
			if err != nil {
				t.Fatal("open external WAL writer")
			}
			defer writer.Close()
			conn, err := writer.Conn(context.Background())
			if err != nil {
				t.Fatal("connect external WAL writer")
			}
			defer conn.Close()
			if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
				t.Fatal("hold external WAL writer")
			}
			defer conn.ExecContext(context.Background(), `ROLLBACK`)
			if operation == "save_password_changed" {
				if _, err := conn.ExecContext(context.Background(), `UPDATE config SET value=? WHERE key='lan_auth_password'`, replacementPassword); err != nil {
					t.Fatal("change password in external writer")
				}
			}
			started := make(chan struct{})
			result := make(chan error, 1)
			begin := time.Now()
			go func() {
				close(started)
				switch operation {
				case "save_first_table", "save_existing_table", "save_password_changed":
					result <- store.SaveLANSession(newDigest, expires, now, password)
				case "delete_one":
					result <- store.DeleteLANSession(digest)
				case "delete_all":
					result <- store.DeleteAllLANSessions()
				}
			}()
			<-started
			select {
			case err := <-result:
				t.Fatalf("session write returned before WAL unlock: error=%t sqlite_code=%d elapsed_ms=%d", err != nil, lanSessionTestErrorCode(err), time.Since(begin).Milliseconds())
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
				t.Fatal("release external WAL writer")
			}
			select {
			case err := <-result:
				if operation == "save_password_changed" {
					if err == nil || err.Error() != "house password changed during login" {
						t.Fatal("session admitted an outdated verified password")
					}
				} else if err != nil {
					t.Fatalf("session write failed after WAL unlock: sqlite_code=%d", lanSessionTestErrorCode(err))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session write did not finish within its existing budget")
			}
			if elapsed := time.Since(begin); elapsed >= 5*time.Second {
				t.Fatalf("session write exceeded its existing budget: elapsed_ms=%d", elapsed.Milliseconds())
			}
			if err := store.Close(); err != nil {
				t.Fatal("close saved store")
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal("reopen saved store")
			}
			defer store.Close()
			sessions, err := store.LoadLANSessions(now)
			if err != nil {
				t.Fatal("load durable sessions")
			}
			want := map[string]int{"save_first_table": 1, "save_existing_table": 3, "save_password_changed": 0, "delete_one": 1, "delete_all": 0}[operation]
			if len(sessions) != want {
				t.Fatalf("durable session count=%d want=%d", len(sessions), want)
			}
			for _, session := range sessions {
				if session.ExpiresMS != expires || (operation == "delete_one" && session.Digest != otherDigest) {
					t.Fatal("durable session expiry or selection changed")
				}
			}
			current, found, err := store.ConfigValue("lan_auth_password")
			wantPassword := password
			if operation == "save_password_changed" {
				wantPassword = replacementPassword
			}
			if err != nil || !found || current != wantPassword {
				t.Fatal("session write changed the stored password")
			}
			if err := store.SaveConfig("lan_auth_password", newLANSessionTestValue(t)); err != nil {
				t.Fatal("change password after reopen")
			}
			sessions, err = store.LoadLANSessions(now)
			if err != nil || len(sessions) != 0 {
				t.Fatal("old sessions were not bound to the saved password")
			}
		})
	}
}

func TestLANSessionWriteFailureRollsBackCleanup(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal("open test store")
	}
	defer store.Close()
	password := newLANSessionTestValue(t)
	digest := newLANSessionTestValue(t)
	expiredDigest := newLANSessionTestValue(t)
	now := time.Now().UnixMilli()
	if err := store.SaveConfig("lan_auth_password", password); err != nil {
		t.Fatal("save test password")
	}
	if err := store.SaveLANSession(expiredDigest, now-1, now-1000, password); err != nil {
		t.Fatal("save soon-expiring session")
	}
	if err := store.SaveLANSession(digest, now+time.Hour.Milliseconds(), now-1000, password); err != nil {
		t.Fatal("save retained session")
	}
	for _, failure := range []string{"password_changed", "duplicate_digest"} {
		t.Run(failure, func(t *testing.T) {
			verified := password
			if failure == "password_changed" {
				verified = newLANSessionTestValue(t)
			}
			if err := store.SaveLANSession(digest, now+time.Hour.Milliseconds(), now, verified); err == nil {
				t.Fatal("invalid session save succeeded")
			}
			var rows int
			if err := store.db.QueryRow(`SELECT count(*) FROM lan_auth_sessions`).Scan(&rows); err != nil || rows != 2 {
				t.Fatal("failed save committed session cleanup")
			}
			current, found, err := store.ConfigValue("lan_auth_password")
			if err != nil || !found || current != password {
				t.Fatal("failed save changed the stored password")
			}
		})
	}
}

func TestLANSessionDeleteBeforeFirstLogin(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal("open test store")
	}
	defer store.Close()
	if err := store.DeleteLANSession(newLANSessionTestValue(t)); err != nil {
		t.Fatal("delete before first login")
	}
	if err := store.DeleteAllLANSessions(); err != nil {
		t.Fatal("delete all before first login")
	}
	var tables int
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lan_auth_sessions'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("delete created a session table")
	}
}
