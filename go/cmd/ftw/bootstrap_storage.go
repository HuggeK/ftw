package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// Publish only a complete first-claim database. Interrupted preparation stays
// in an owned temporary directory, never at the authoritative state.db path.
func saveInitialAssistantStorage(path string, cfg *config.Config) error {
	if cfg.LANPasswordHash == "" {
		return fmt.Errorf("initial house password is missing")
	}
	parent, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	database := filepath.Join(parent, "state.db")
	dir, err := os.MkdirTemp(parent, ".ftw-initial-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	staging := filepath.Join(dir, "prepare.db")
	st, err := state.CreateInitialSettings(staging)
	if err != nil {
		return err
	}
	defer st.Close()
	cfg.ConfigDatabase = staging
	if err := config.SaveStored(st, path, cfg); err != nil {
		return err
	}
	complete := filepath.Join(dir, "complete.db")
	if err := st.SnapshotInitialSettings(complete); err != nil {
		return err
	}
	document, err := state.ReadConfiguration(complete)
	if err != nil || document.Revision != cfg.Revision {
		return fmt.Errorf("initial settings were not captured")
	}
	if err := os.Chmod(complete, 0600); err != nil {
		return err
	}
	file, err := os.OpenFile(complete, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link is an atomic create-if-absent on this same filesystem. It cannot
	// replace another install, including one created after the first check.
	if err := os.Link(complete, database); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	_ = directory.Close()
	if syncErr != nil {
		return fmt.Errorf("initial settings published but directory sync failed: %w", syncErr)
	}
	cfg.ConfigDatabase = database
	if err := config.SaveAtomic(path, cfg); err != nil {
		// The transaction is already durable. Load recovers Settings beside a
		// missing locator; do not ask the owner to repeat a successful claim.
		loaded, recoveryErr := config.Load(path)
		if recoveryErr != nil || loaded.Revision != cfg.Revision || loaded.LANPasswordHash != cfg.LANPasswordHash {
			return err
		}
		slog.Warn("initial settings saved; startup will repair the settings locator", "err", err)
	}
	return nil
}

// sameSetupMeterConfiguration prevents commissioning with readings from a
// different endpoint or scaling than the configuration being enabled.
func sameSetupMeterConfiguration(active, candidate *config.Config) bool {
	name := candidate.SiteMeterDriver()
	if name == "" || active.SiteMeterDriver() != name {
		return false
	}
	var before, after *config.Driver
	for i := range active.Drivers {
		if active.Drivers[i].Name == name {
			before = &active.Drivers[i]
		}
	}
	for i := range candidate.Drivers {
		if candidate.Drivers[i].Name == name {
			after = &candidate.Drivers[i]
		}
	}
	return before != nil && after != nil && reflect.DeepEqual(*before, *after)
}
