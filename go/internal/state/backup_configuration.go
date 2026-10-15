package state

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RewriteBackupConfiguration changes only settings in an extracted offline
// copy. It does not create tables, run migrations or open history files.
func RewriteBackupConfiguration(path string, rewrite func([]byte) ([]byte, error)) error {
	uri := strings.Replace(ReadOnlyDatabaseURI(path), "mode=ro", "mode=rw", 1)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Leave a self-contained database for the later directory swap.
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='config'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil
	}
	var raw string
	if err := tx.QueryRow(`SELECT value FROM config WHERE key=?`, configurationKey).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	c, err := decodeConfiguration(raw)
	if err != nil {
		return err
	}
	doc, err := rewrite(c.Document)
	if err != nil {
		return err
	}
	if bytes.Equal(doc, c.Document) {
		return nil
	}
	if !json.Valid(doc) {
		return fmt.Errorf("backup settings rewrite returned invalid JSON")
	}
	// Keep the stored version, revision and any unknown wrapper fields.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return err
	}
	wrapper["document"] = doc
	encoded, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE config SET value=? WHERE key=?`, string(encoded), configurationKey); err != nil {
		return err
	}
	return tx.Commit()
}

// BackupDriverFiles reads the stored identities from an offline backup copy.
// It does not create tables or change the database.
func BackupDriverFiles(path string) (map[string]string, error) {
	db, err := sql.Open("sqlite", ReadOnlyDatabaseURI(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='driver_repo_installs'`).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, nil
	}
	rows, err := db.Query(`SELECT installed_path, sha256 FROM driver_repo_installs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make(map[string]string)
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, err
		}
		if prior, ok := files[name]; ok && prior != hash {
			return nil, fmt.Errorf("backup driver path has conflicting hashes: %s", name)
		}
		files[name] = hash
	}
	return files, rows.Err()
}

// RewriteBackupDriverPaths maps durable selections and their undo paths in an
// offline staging copy. It preserves activation, provenance and version history.
func RewriteBackupDriverPaths(path string, rewrite func(string) (string, error)) error {
	uri := strings.Replace(ReadOnlyDatabaseURI(path), "mode=ro", "mode=rw", 1)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='driver_repo_installs'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil
	}
	rows, err := tx.Query(`SELECT id, installed_path, previous_installed_path FROM driver_repo_installs`)
	if err != nil {
		return err
	}
	type paths struct {
		id                  int64
		installed, previous string
	}
	var changes []paths
	for rows.Next() {
		var p paths
		if err := rows.Scan(&p.id, &p.installed, &p.previous); err != nil {
			rows.Close()
			return err
		}
		installed, err := rewrite(p.installed)
		if err != nil {
			rows.Close()
			return fmt.Errorf("driver repository installed path: %w", err)
		}
		previous, err := rewrite(p.previous)
		if err != nil {
			rows.Close()
			return fmt.Errorf("driver repository undo path: %w", err)
		}
		if installed != p.installed || previous != p.previous {
			changes = append(changes, paths{p.id, installed, previous})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range changes {
		if _, err := tx.Exec(`UPDATE driver_repo_installs SET installed_path=?, previous_installed_path=? WHERE id=?`, p.installed, p.previous, p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
