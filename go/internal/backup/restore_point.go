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
const restoreStageFile = ".ftw-restore-stage-record.json"

type restoreStage struct {
	Schema int    `json:"schema"`
	Phase  string `json:"phase"`
	Safety string `json:"safety,omitempty"`
}

type restorePoint struct {
	Schema   int      `json:"schema"`
	Complete bool     `json:"complete"`
	Entries  []string `json:"entries"`
}

// Record the file list before moving anything. A failed or interrupted write
// remains unconfirmed; revert must not treat it as a complete previous state.
func writeRestorePoint(dir string, entries []string, complete bool) error {
	return writeRestoreRecord(dir, restorePointFile, restorePoint{Schema: 1, Complete: complete, Entries: entries})
}

func writeRestoreRecord(dir, name string, record any) error {
	if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && !info.Mode().IsRegular() {
		return errors.New("restore record is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(file).Encode(record)
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

// Associate each stage with its own phase before moving active files. Older
// complete safety points do not imply that a later extraction moved data.
func writeRestoreStage(dataDir, dir, safety string) error {
	phase := "extracting"
	if safety != "" {
		phase = "preserving"
	}
	if err := writeRestoreRecord(dir, restoreStageFile, restoreStage{Schema: 1, Phase: phase, Safety: safety}); err != nil {
		return err
	}
	return syncDir(dataDir)
}

func readRestoreStage(dir string) (restoreStage, error) {
	var stage restoreStage
	name := filepath.Join(dir, restoreStageFile)
	info, err := os.Lstat(name)
	if err != nil {
		return stage, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return stage, errors.New("invalid stage record")
	}
	f, err := os.Open(name)
	if err != nil {
		return stage, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&stage); err != nil {
		return stage, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return stage, errors.New("invalid trailing stage record")
	}
	if stage.Schema != 1 || (stage.Phase != "extracting" && stage.Phase != "preserving") {
		return stage, errors.New("unknown stage phase")
	}
	if stage.Phase == "extracting" && stage.Safety != "" {
		return stage, errors.New("extraction already names a safety point")
	}
	if stage.Phase == "preserving" && (!strings.HasPrefix(stage.Safety, ".ftw-pre-restore-") || strings.ContainsAny(stage.Safety, `/\`)) {
		return stage, errors.New("invalid stage safety reference")
	}
	return stage, nil
}

// A retry must not certify a partially moved active directory as complete.
// Legacy stages without records remain retryable only when no safety point
// could be associated with them. Unknown or changed records always stop.
func checkRestorePointsBeforeRestore(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	points := 0
	unresolved := func(path string, err error) error {
		return fmt.Errorf("backup: unresolved restore operation %s; retain all directories and recover a complete point or a full backup on a separate target: %w", path, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dataDir, entry.Name())
		if strings.HasPrefix(entry.Name(), ".ftw-rejected-restore-") {
			return unresolved(path, errors.New("revert did not finish"))
		}
		if !strings.HasPrefix(entry.Name(), ".ftw-pre-restore-") {
			continue
		}
		points++
		if !entry.IsDir() {
			return unresolved(path, errors.New("point is not a directory"))
		}
		if err := verifyRestorePoint(path); err != nil {
			return unresolved(path, err)
		}
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".ftw-restore-stage-") {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		if !entry.IsDir() {
			return unresolved(path, errors.New("stage is not a directory"))
		}
		stage, err := readRestoreStage(path)
		if errors.Is(err, os.ErrNotExist) && points == 0 {
			continue
		}
		if err != nil {
			return unresolved(path, err)
		}
		if stage.Phase != "extracting" {
			return unresolved(path, fmt.Errorf("preservation started for %s", stage.Safety))
		}
	}
	return nil
}

func currentRestoreStateComplete(dataDir string) bool {
	return checkRestorePointsBeforeRestore(dataDir) == nil
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

// Retain old extraction copies under a distinct name once a retry has fully
// activated its verified archive. They no longer describe an active operation.
func retireRestoreStaging(dataDir, currentStage string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".ftw-restore-stage-") || entry.Name() == filepath.Base(currentStage) {
			continue
		}
		from := filepath.Join(dataDir, entry.Name())
		to := filepath.Join(dataDir, ".ftw-abandoned-stage-"+strings.TrimPrefix(entry.Name(), ".ftw-restore-stage-"))
		if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: retained extraction path already exists: %s", to)
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return syncDir(dataDir)
}
