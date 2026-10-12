package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRevertRejectsInterruptedPreservationWithoutMovingFiles(t *testing.T) {
	for _, receipt := range []string{"", `{"schema":1,"complete":false,"entries":["first","second"]}`} {
		t.Run(receipt, func(t *testing.T) {
			data := t.TempDir()
			safety := filepath.Join(data, ".ftw-pre-restore-proof")
			writeTestFile(t, filepath.Join(safety, "first"), "original first")
			writeTestFile(t, filepath.Join(data, "second"), "original second")
			if receipt != "" {
				writeTestFile(t, filepath.Join(safety, ".ftw-restore-point.json"), receipt)
			}
			if _, err := RevertContents(data, safety); err == nil {
				t.Fatal("revert accepted an incomplete or unrecorded restore point")
			}
			// An older unrecorded point is also refused without staging: an
			// older error path may have cleaned staging after a failed rollback.
			if _, err := RestoreContents("unused.ftwbak", data, time.Time{}); err == nil || !strings.Contains(err.Error(), "unresolved restore point") {
				t.Fatalf("retry did not reject the incomplete root before import: %v", err)
			}
			for path, want := range map[string]string{
				filepath.Join(safety, "first"): "original first",
				filepath.Join(data, "second"):  "original second",
			} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("rejected revert changed %s: %q %v", path, got, err)
				}
			}
		})
	}
}

func TestRevertRecoversCompleteOriginalFromInterruptedActivation(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "old"), "complete original")
	writeTestFile(t, filepath.Join(data, "new"), "partly activated")
	if err := os.Mkdir(filepath.Join(data, ".ftw-restore-stage-proof"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRestorePoint(safety, []string{"old"}, true); err != nil {
		t.Fatal(err)
	}
	result, err := RevertContents(data, safety)
	if err != nil || result.SafetyComplete {
		t.Fatalf("recovery certified the incomplete rejected state: %+v %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "old")); err != nil || string(got) != "complete original" {
		t.Fatalf("original not recovered: %q %v", got, err)
	}
	if _, err := RevertContents(data, result.SafetyDir); err == nil {
		t.Fatal("revert accepted the retained incomplete target")
	}
	if got, err := os.ReadFile(filepath.Join(result.SafetyDir, "new")); err != nil || string(got) != "partly activated" {
		t.Fatalf("incomplete target not retained: %q %v", got, err)
	}
}

func TestRevertKeepsRejectedDataWhenRollbackCannotMoveIt(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "old"), "original data")
	writeTestFile(t, filepath.Join(data, "new"), "new readings absent from the backup")
	if err := writeRestorePoint(safety, []string{"old"}, true); err != nil {
		t.Fatal(err)
	}
	realMove := moveRestoreEntry
	t.Cleanup(func() { moveRestoreEntry = realMove })
	moveRestoreEntry = func(from, to string) error {
		if from == filepath.Join(safety, "old") || strings.HasPrefix(filepath.Base(filepath.Dir(from)), ".ftw-rejected-restore-") {
			return errors.New("injected move failure")
		}
		return realMove(from, to)
	}
	if _, err := RevertContents(data, safety); err == nil {
		t.Fatal("revert unexpectedly succeeded")
	}
	rejected, err := filepath.Glob(filepath.Join(data, ".ftw-rejected-restore-*", "new"))
	if err != nil || len(rejected) != 1 {
		t.Fatalf("failed rollback lost the current data: %v %v", rejected, err)
	}
	if got, err := os.ReadFile(rejected[0]); err != nil || string(got) != "new readings absent from the backup" {
		t.Fatalf("failed rollback changed the current data: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(safety, "old")); err != nil || string(got) != "original data" {
		t.Fatalf("failed rollback changed original data: %q %v", got, err)
	}
}

func TestFailedRevertRollbackDoesNotOverwriteTheOriginal(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "a-shared"), "original value")
	writeTestFile(t, filepath.Join(safety, "b-fail"), "other original")
	writeTestFile(t, filepath.Join(data, "a-shared"), "new value")
	if err := writeRestorePoint(safety, []string{"a-shared", "b-fail"}, true); err != nil {
		t.Fatal(err)
	}
	realMove := moveRestoreEntry
	t.Cleanup(func() { moveRestoreEntry = realMove })
	moveRestoreEntry = func(from, to string) error {
		if from == filepath.Join(safety, "b-fail") || (from == filepath.Join(data, "a-shared") && to == filepath.Join(safety, "a-shared")) {
			return errors.New("injected move failure")
		}
		return realMove(from, to)
	}
	if _, err := RevertContents(data, safety); err == nil {
		t.Fatal("revert unexpectedly succeeded")
	}
	if got, err := os.ReadFile(filepath.Join(data, "a-shared")); err != nil || string(got) != "original value" {
		t.Fatalf("rollback overwrote the original: %q %v", got, err)
	}
	rejected, err := filepath.Glob(filepath.Join(data, ".ftw-rejected-restore-*", "a-shared"))
	if err != nil || len(rejected) != 1 {
		t.Fatalf("new value not retained: %v %v", rejected, err)
	}
	if got, err := os.ReadFile(rejected[0]); err != nil || string(got) != "new value" {
		t.Fatalf("new value changed: %q %v", got, err)
	}
}

func TestRevertRejectsMalformedAndLinkedRecords(t *testing.T) {
	for _, body := range []string{
		`{"schema":1,"complete":true,"entries":["../old"]}`,
		`{"schema":1,"complete":true,"entries":["old","old"]}`,
		`{"schema":1,"complete":true,"entries":["old"]} trailing`,
		`{"schema":2,"complete":true,"entries":["old"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			data := t.TempDir()
			safety := filepath.Join(data, ".ftw-pre-restore-proof")
			writeTestFile(t, filepath.Join(safety, "old"), "old")
			writeTestFile(t, filepath.Join(safety, restorePointFile), body)
			if _, err := RevertContents(data, safety); err == nil {
				t.Fatal("revert accepted a malformed record")
			}
		})
	}
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "old"), "old")
	external := filepath.Join(t.TempDir(), "external.json")
	writeTestFile(t, external, `{"schema":1,"complete":true,"entries":["old"]}`)
	if err := os.Symlink(external, filepath.Join(safety, restorePointFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := RevertContents(data, safety); err == nil {
		t.Fatal("revert accepted a linked record")
	}
}

func TestRevertRejectsMissingRecordedEntry(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "first"), "original first")
	writeTestFile(t, filepath.Join(data, "new"), "new active data")
	writeTestFile(t, filepath.Join(safety, ".ftw-restore-point.json"), `{"schema":1,"complete":true,"entries":["first","missing"]}`)
	if _, err := RevertContents(data, safety); err == nil {
		t.Fatal("revert accepted a point missing a recorded entry")
	}
	if got, err := os.ReadFile(filepath.Join(data, "new")); err != nil || string(got) != "new active data" {
		t.Fatalf("rejected revert moved current data: %q %v", got, err)
	}
}

func TestRetryRevertDoesNotCertifyEmptyActiveRoot(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	rejected := filepath.Join(data, ".ftw-rejected-restore-interrupted")
	writeTestFile(t, filepath.Join(safety, "old"), "original data")
	writeTestFile(t, filepath.Join(rejected, "new"), "new readings")
	if err := writeRestorePoint(safety, []string{"old"}, true); err != nil {
		t.Fatal(err)
	}
	if err := writeRestorePoint(rejected, []string{"new"}, true); err != nil {
		t.Fatal(err)
	}
	result, err := RevertContents(data, safety)
	if err != nil || result.SafetyComplete {
		t.Fatalf("retry certified empty root: %+v %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "old")); err != nil || string(got) != "original data" {
		t.Fatalf("old data not recovered: %q %v", got, err)
	}
	if _, err := RevertContents(data, safety); err == nil {
		t.Fatal("revert activated empty state")
	}
	if got, err := os.ReadFile(filepath.Join(rejected, "new")); err != nil || string(got) != "new readings" {
		t.Fatalf("new data not retained: %q %v", got, err)
	}
}

func TestRetryImportRejectsInterruptedActivationWithCompletePoint(t *testing.T) {
	data := t.TempDir()
	safety := filepath.Join(data, ".ftw-pre-restore-proof")
	writeTestFile(t, filepath.Join(safety, "old"), "complete original")
	writeTestFile(t, filepath.Join(data, "new"), "partial target")
	writeTestFile(t, filepath.Join(data, ".ftw-restore-stage-proof", "pending"), "unactivated target")
	if err := writeRestorePoint(safety, []string{"old"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreContents("unused.ftwbak", data, time.Time{}); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("import certified partial activation: %v", err)
	}
}
