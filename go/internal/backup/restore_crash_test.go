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
	if _, err := RestoreContents(os.Getenv("FTW_RESTORE_CRASH_ARCHIVE"), data, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("restore completed without reaching the crash window")
}

func TestRestoreProcessCrashPreservesDataAndRefusesPartialPoints(t *testing.T) {
	for _, phase := range []string{"preserving", "activating"} {
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
			marker := filepath.Join(root, "crash-window")
			child := exec.Command(os.Args[0], "-test.run=^TestRestoreCrashChild$", "-test.count=1")
			child.Env = append(os.Environ(), "FTW_RESTORE_CRASH_PHASE="+phase, "FTW_RESTORE_CRASH_DATA="+data, "FTW_RESTORE_CRASH_ARCHIVE="+archive.Path, "FTW_RESTORE_CRASH_MARKER="+marker)
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
				if phase == "activating" && (rootErr != nil || string(atRoot) != "original data") {
					t.Fatalf("original %s not active after recovery", name)
				}
				if (rootErr != nil || string(atRoot) != "original data") && (pointErr != nil || string(atPoint) != "original data") {
					t.Fatalf("original %s lost after %s crash", name, phase)
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
