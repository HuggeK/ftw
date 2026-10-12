package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const restorePointFile = ".ftw-restore-point.json"

type restorePoint struct {
	Schema   int      `json:"schema"`
	Complete bool     `json:"complete"`
	Entries  []string `json:"entries"`
}

// Record the file list before moving anything. A failed or interrupted write
// remains unconfirmed; revert must not treat it as a complete previous state.
func writeRestorePoint(dir string, entries []string, complete bool) error {
	file, err := os.OpenFile(filepath.Join(dir, restorePointFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(file).Encode(restorePoint{Schema: 1, Complete: complete, Entries: entries})
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close(), syncDir(dir))
}

func completeRestorePoint(dataDir, pointDir string, entries []string) error {
	if err := errors.Join(syncDir(dataDir), syncDir(pointDir)); err != nil {
		return err
	}
	return writeRestorePoint(pointDir, entries, true)
}

func verifyRestorePoint(dir string) error {
	refuse := func(err error) error {
		return fmt.Errorf("backup: restore point is incomplete or unverified; keep FTW stopped, retain both directories and recover from a verified full backup: %w", err)
	}
	name := filepath.Join(dir, restorePointFile)
	info, err := os.Lstat(name)
	if err != nil {
		return refuse(err)
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return refuse(errors.New("invalid restore point record"))
	}
	file, err := os.Open(name)
	if err != nil {
		return refuse(err)
	}
	defer file.Close()
	var point restorePoint
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&point); err != nil {
		return refuse(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return refuse(errors.New("invalid trailing restore point data"))
	}
	if point.Schema != 1 || !point.Complete {
		return refuse(errors.New("previous files were not fully preserved"))
	}
	for i, entry := range point.Entries {
		if entry == "" || entry == "." || entry == ".." || strings.ContainsAny(entry, `/\`) ||
			isRestoreInternalName(entry) || (i > 0 && entry <= point.Entries[i-1]) {
			return refuse(errors.New("invalid previous file list"))
		}
	}
	entries, err := topLevelEntries(dir)
	if err != nil {
		return refuse(err)
	}
	if !slices.Equal(entries, point.Entries) {
		return refuse(errors.New("previous files do not match their recorded list"))
	}
	return nil
}

func hasRestoreStaging(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ftw-restore-stage-") {
			return true
		}
	}
	return false
}

// A retry must not certify a partially moved active directory as a new,
// complete previous state. Interrupted extraction alone has no safety point
// and can still be retried. Unrecorded older points require a separate target.
func checkRestorePointsBeforeRestore(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".ftw-pre-restore-") && !strings.HasPrefix(entry.Name(), ".ftw-rejected-restore-") {
			continue
		}
		point := filepath.Join(dataDir, entry.Name())
		if !entry.IsDir() {
			return fmt.Errorf("backup: unresolved restore point %s; retain the data and restore a full backup into a separate directory", point)
		}
		if err := verifyRestorePoint(point); err != nil {
			return fmt.Errorf("backup: unresolved restore point %s; retain the data and restore a full backup into a separate directory: %w", point, err)
		}
	}
	return nil
}

func currentRestoreStateComplete(dataDir string) bool {
	entries, err := os.ReadDir(dataDir)
	return err == nil && !hasRestoreStaging(entries) && checkRestorePointsBeforeRestore(dataDir) == nil
}

func rollbackRestorePoint(pointDir, dataDir string, moved []string) error {
	if err := restoreMoveSet(pointDir, dataDir, moved); err != nil {
		return err
	}
	// Only our record may be removed here. Never clean up retained file data
	// merely because a rollback appeared to finish.
	entries, err := os.ReadDir(pointDir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != restorePointFile {
		return errors.New("rollback left files in its retained restore point")
	}
	if err := os.Remove(filepath.Join(pointDir, restorePointFile)); err != nil {
		return err
	}
	return os.Remove(pointDir)
}
