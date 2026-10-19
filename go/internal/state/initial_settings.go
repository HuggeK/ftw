package state

import (
	"context"
	"errors"
	"os"
)

// SnapshotInitialSettings produces a complete SQLite file for atomic first-
// install publication. It is not a history export or an existing-home backup.
func (s *Store) SnapshotInitialSettings(path string) error {
	doc, found, err := s.Configuration()
	if err != nil {
		return err
	}
	if !found || doc.Revision != 1 {
		return errors.New("initial settings snapshot requires the first committed revision")
	}
	return s.copyStateForBackup(context.Background(), path)
}

// CreateInitialSettings prepares only settings in a newly owned staging file.
// History is created by the first real boot, beside the published state.db.
// Opening a full Store here would bind a history file in the staging directory.
func CreateInitialSettings(path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := openRaw(path)
	if err != nil {
		return nil, err
	}
	cache, err := openRaw(path + ".cache")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, cache: cache, mainDBPath: path}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
