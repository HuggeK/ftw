//go:build !windows

package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestRestoreCrashChild(t *testing.T) {
	phase := os.Getenv("FTW_RESTORE_CRASH_PHASE")
	if phase == "" {
		return
	}
	data := os.Getenv("FTW_RESTORE_CRASH_DATA")
	marker := os.Getenv("FTW_RESTORE_CRASH_MARKER")
	realMove := moveRestoreEntry
	moved := 0
	moveRestoreEntry = func(from, to string) error {
		if phase == "reverting" && filepath.Dir(to) == data && strings.HasPrefix(filepath.Base(filepath.Dir(from)), ".ftw-pre-restore-") {
			if err := os.WriteFile(marker, []byte(filepath.Dir(from)), 0o600); err != nil {
				return err
			}
			for {
				time.Sleep(time.Minute)
			}
		}
		if err := realMove(from, to); err != nil {
			return err
		}
		preserving := filepath.Dir(from) == data && strings.HasPrefix(filepath.Base(filepath.Dir(to)), ".ftw-pre-restore-")
		activating := filepath.Dir(to) == data && strings.HasPrefix(filepath.Base(filepath.Dir(from)), ".ftw-restore-stage-")
		if (phase == "preserving" && preserving) || (phase == "activating" && activating) {
			moved++
			if moved == 64 {
				if err := os.WriteFile(marker, []byte(filepath.Dir(to)), 0o600); err != nil {
					return err
				}
				// Hold a real, partly completed filesystem transaction for the
				// parent's SIGKILL without depending on scheduler speed.
				for {
					time.Sleep(time.Minute)
				}
			}
		}
		return nil
	}
	if phase == "reverting" {
		if _, err := RevertContents(data, os.Getenv("FTW_RESTORE_CRASH_SAFETY")); err != nil {
			t.Fatal(err)
		}
		t.Fatal("revert completed without reaching the crash window")
	}
	if _, err := RestoreContents(os.Getenv("FTW_RESTORE_CRASH_ARCHIVE"), data, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("restore completed without reaching the crash window")
}

func TestRestoreProcessCrashPreservesDataAndRefusesPartialPoints(t *testing.T) {
	for _, phase := range []string{"preserving", "activating", "reverting"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			st, err := state.Open(filepath.Join(source, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.SaveConfig("goal", "synthetic saved goal"); err != nil {
				t.Fatal(err)
			}
			for i := range 128 {
				writeTestFile(t, filepath.Join(source, fmt.Sprintf("%04d-new", i)), "new data")
			}
			archive, err := Create(context.Background(), CreateOptions{State: st, StatePath: filepath.Join(source, "state.db"), DataDir: source, OutputDir: filepath.Join(root, "backups")})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(root, "target")
			for i := range 128 {
				writeTestFile(t, filepath.Join(data, fmt.Sprintf("%04d-old", i)), "original data")
			}
			safety := ""
			if phase == "reverting" {
				result, err := RestoreContents(archive.Path, data, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				safety = result.SafetyDir
			}
			marker := filepath.Join(root, "crash-window")
			child := exec.Command(os.Args[0], "-test.run=^TestRestoreCrashChild$", "-test.count=1")
			child.Env = append(os.Environ(), "FTW_RESTORE_CRASH_PHASE="+phase, "FTW_RESTORE_CRASH_DATA="+data, "FTW_RESTORE_CRASH_ARCHIVE="+archive.Path, "FTW_RESTORE_CRASH_MARKER="+marker, "FTW_RESTORE_CRASH_SAFETY="+safety)
			log, err := os.Create(filepath.Join(root, "child.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			child.Stdout, child.Stderr = log, log
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					body, _ := os.ReadFile(filepath.Join(root, "child.log"))
					t.Fatalf("crash window not reached: %s", body)
				}
				time.Sleep(time.Millisecond)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("child survived SIGKILL")
			} else if status, ok := child.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not terminate through SIGKILL: %v", child.ProcessState)
			}
			points, err := filepath.Glob(filepath.Join(data, ".ftw-pre-restore-*"))
			if err != nil || len(points) != 1 {
				t.Fatalf("missing retained point: %v %v", points, err)
			}
			point := points[0]
			if phase != "reverting" {
				stages, err := filepath.Glob(filepath.Join(data, ".ftw-restore-stage-*"))
				if err != nil || len(stages) != 1 {
					t.Fatalf("stage association missing: %v %v", stages, err)
				}
				stage, err := readRestoreStage(stages[0])
				if err != nil || stage.Phase != "preserving" || stage.Safety != filepath.Base(point) {
					t.Fatalf("stage not bound to its point: %+v %v", stage, err)
				}
			}
			if phase == "reverting" {
				active, err := topLevelEntries(data)
				if err != nil || len(active) != 0 {
					t.Fatalf("revert crash did not leave the empty-root window: %v %v", active, err)
				}
				if err := verifyRestorePoint(point); err != nil {
					t.Fatal(err)
				}
				rejected, err := filepath.Glob(filepath.Join(data, ".ftw-rejected-restore-*"))
				if err != nil || len(rejected) != 1 {
					t.Fatalf("rejected data missing: %v %v", rejected, err)
				}
				if err := verifyRestorePoint(rejected[0]); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "preserving" {
				if _, err := RevertContents(data, point); err == nil {
					t.Fatal("partial preservation was accepted")
				}
				if _, err := RestoreContents(archive.Path, data, time.Now()); err == nil {
					t.Fatal("partial active root was certified by a retry")
				}
			} else {
				result, err := RevertContents(data, point)
				if err != nil || result.SafetyComplete {
					t.Fatalf("complete source did not recover with partial target retained: %+v %v", result, err)
				}
				if _, err := RevertContents(data, point); err == nil {
					t.Fatal("partial target was accepted for reactivation")
				}
			}
			for i := range 128 {
				name := fmt.Sprintf("%04d-old", i)
				atRoot, rootErr := os.ReadFile(filepath.Join(data, name))
				atPoint, pointErr := os.ReadFile(filepath.Join(point, name))
				if phase != "preserving" && (rootErr != nil || string(atRoot) != "original data") {
					t.Fatalf("original %s not active after recovery", name)
				}
				if (rootErr != nil || string(atRoot) != "original data") && (pointErr != nil || string(atPoint) != "original data") {
					t.Fatalf("original %s lost after %s crash", name, phase)
				}
			}
			if phase == "reverting" {
				rejected, _ := filepath.Glob(filepath.Join(data, ".ftw-rejected-restore-*"))
				for i := range 128 {
					got, err := os.ReadFile(filepath.Join(rejected[0], fmt.Sprintf("%04d-new", i)))
					if err != nil || string(got) != "new data" {
						t.Fatalf("revert crash lost new data %d: %q %v", i, got, err)
					}
				}
			}
			fresh := filepath.Join(root, "fresh-recovery")
			if err := os.Mkdir(fresh, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := RestoreContents(archive.Path, fresh, time.Now()); err != nil {
				t.Fatal("full backup did not recover on a separate copy:", err)
			}
		})
	}
}
