package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// LANSession stores a digest of an opaque browser token, never the token.
// Bind it to the password that admitted it, so a password change invalidates
// old rows even if the process stops before their cleanup commits.
type LANSession struct {
	Digest    string
	ExpiresMS int64
}

func lanPasswordBinding(encoded string) string {
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:])
}

// Take the WAL writer before any read, including an existing-table schema
// check. A deferred read transaction cannot wait when upgrading to a writer.
// This leaves the stored password unchanged.
func lockLANSessionWrite(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE config SET value=value WHERE key='lan_auth_password'`)
	return err
}

func (s *Store) SaveLANSession(digest string, expiresMS, nowMS int64, verifiedPassword string) error {
	if len(digest) != 64 || verifiedPassword == "" || expiresMS <= nowMS {
		return errors.New("invalid LAN session")
	}
	return s.durableConfigWrite(func(tx *sql.Tx) error {
		if err := lockLANSessionWrite(tx); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRow(`SELECT value FROM config WHERE key='lan_auth_password'`).Scan(&current); err != nil {
			return err
		}
		if current != verifiedPassword {
			return errors.New("house password changed during login")
		}
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS lan_auth_sessions (
			token_digest TEXT PRIMARY KEY, expires_ms INTEGER NOT NULL, password_binding TEXT NOT NULL)`); err != nil {
			return err
		}
		binding := lanPasswordBinding(current)
		if _, err := tx.Exec(`DELETE FROM lan_auth_sessions WHERE expires_ms <= ? OR password_binding <> ?`, nowMS, binding); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO lan_auth_sessions(token_digest,expires_ms,password_binding) VALUES(?,?,?)`, digest, expiresMS, binding)
		return err
	})
}

func (s *Store) LoadLANSessions(nowMS int64) ([]LANSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lan_auth_sessions'`).Scan(&exists); err != nil || exists == 0 {
		return nil, err
	}
	var password string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key='lan_auth_password'`).Scan(&password); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if password == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token_digest,expires_ms FROM lan_auth_sessions WHERE expires_ms > ? AND password_binding = ?`, nowMS, lanPasswordBinding(password))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []LANSession
	for rows.Next() {
		var session LANSession
		if err := rows.Scan(&session.Digest, &session.ExpiresMS); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) DeleteLANSession(digest string) error {
	return s.deleteLANSessions(&digest)
}

func (s *Store) DeleteAllLANSessions() error {
	return s.deleteLANSessions(nil)
}

func (s *Store) deleteLANSessions(digest *string) error {
	return s.durableConfigWrite(func(tx *sql.Tx) error {
		if err := lockLANSessionWrite(tx); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lan_auth_sessions'`).Scan(&exists); err != nil || exists == 0 {
			return err
		}
		if digest == nil {
			_, err := tx.Exec(`DELETE FROM lan_auth_sessions`)
			return err
		}
		_, err := tx.Exec(`DELETE FROM lan_auth_sessions WHERE token_digest=?`, *digest)
		return err
	})
}
