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
