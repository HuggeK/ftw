package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/driverrepo"
	"github.com/srcfl/ftw/go/internal/state"
)

func managedBackupFixture(t *testing.T, previousExternal bool, defaults ...bool) (string, string) {
	t.Helper()
	source := t.TempDir()
	root := filepath.Join(source, "custom-repository")
	database := filepath.Join(source, "state.db")
	seed := filepath.Join(source, "config.yaml")
	text := "site:\n  name: Driver migration\nfuse:\n  max_amps: 16\nstate:\n  path: " + database + "\ndevice_repository:\n  enabled: true\n  root_dir: " + root + "\n"
	if len(defaults) > 0 && defaults[0] {
		root = filepath.Join(source, "driver-repository")
		text = "site:\n  name: Driver migration\nfuse:\n  max_amps: 16\ndevice_repository:\n  enabled: true\n"
	}
	writeTestFile(t, seed, text)
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Load(seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.InitializeStorage(seed, database, cfg, st); err != nil {
		t.Fatal(err)
	}
	var chosen state.DriverRepoInstall
	for _, version := range []string{"1.0.0", "2.0.0"} {
		path := filepath.Join(root, "installed", version, "meter.lua")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := []byte("DRIVER={id='migration-meter',version='" + version + "'}")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(body)
		installed := path
		if previousExternal && version == "1.0.0" {
			installed = "/external/old-meter.lua"
		}
		chosen, err = st.ActivateDriverRepoInstall(state.DriverRepoInstall{RepoID: "fixture", RepoURL: "https://example.invalid/manifest.json", DriverID: "migration-meter", LogicalPath: "drivers/meter.lua", Version: version, SHA256: hex.EncodeToString(hash[:]), InstalledPath: installed, RepositoryFormat: "lua-driver"})
		if err != nil {
			t.Fatal(err)
		}
	}
	active := filepath.Join(root, "active", "meter.lua")
	if err := os.MkdirAll(filepath.Dir(active), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(chosen.InstalledPath, active); err != nil {
		t.Fatal(err)
	}
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: database, DataDir: source, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return source, info.Path
}

func TestRestoreOldManagedBackupWithDefaultPaths(t *testing.T) {
	source, archive := managedBackupFixture(t, false, true)
	archive = withoutRelocationMetadata(t, archive)
	target := t.TempDir()
	result, err := RestoreContents(archive, target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err := config.Load(filepath.Join(target, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := state.Open(filepath.Join(target, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		m := driverrepo.New(cfg.DeviceRepository, target, st)
		active, err := st.ActiveDriverRepoInstall("drivers/meter.lua")
		want := filepath.Join(target, "driver-repository", "installed", "2.0.0", "meter.lua")
		prev := filepath.Join(target, "driver-repository", "installed", "1.0.0", "meter.lua")
		if err != nil || active.InstalledPath != want || active.PreviousInstalledPath != prev || active.Version != "2.0.0" {
			st.Close()
			t.Fatalf("default-path selection at start %d: %+v %v", i, active, err)
		}
		path, err := filepath.EvalSymlinks(filepath.Join(m.EffectiveDir(), "meter.lua"))
		resolvedWant, _ := filepath.EvalSymlinks(want)
		st.Close()
		if err != nil || path != resolvedWant {
			t.Fatalf("effective driver: %s %v", path, err)
		}
	}
	if _, err := RevertContents(target, result.SafetyDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "driver-repository", "installed", "2.0.0", "meter.lua")); err != nil {
		t.Fatal("source changed:", err)
	}
}

func TestRestoreOldManagedBackupRejectsUnboundRoot(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-hash", true: "ambiguous-root"}[ambiguous], func(t *testing.T) {
			source, _ := managedBackupFixture(t, false, true)
			for _, version := range []string{"1.0.0", "2.0.0"} {
				path := filepath.Join(source, "driver-repository", "installed", version, "meter.lua")
				if ambiguous {
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(source, "installed", version, "meter.lua"), string(body))
				} else {
					writeTestFile(t, path, "different driver bytes")
				}
			}
			st, err := state.Open(filepath.Join(source, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := Create(context.Background(), CreateOptions{State: st, StatePath: filepath.Join(source, "state.db"), DataDir: source, OutputDir: t.TempDir()})
			st.Close()
			if err != nil {
				t.Fatal(err)
			}
			archive := withoutRelocationMetadata(t, info.Path)
			target := t.TempDir()
			writeTestFile(t, filepath.Join(target, "prior-target"), "keep")
			if _, err := RestoreContents(archive, target, time.Now()); err == nil {
				t.Fatal("accepted unbound driver root")
			}
			body, _ := os.ReadFile(filepath.Join(target, "prior-target"))
			entries, _ := os.ReadDir(target)
			if string(body) != "keep" || len(entries) != 1 {
				t.Fatal("failed restore changed target")
			}
		})
	}
}

func TestRestoreKeepsManagedSelectionAndUndoPaths(t *testing.T) {
	for _, same := range []bool{true, false} {
		for _, old := range []bool{true, false} {
			t.Run(map[bool]string{true: "same-root", false: "new-root"}[same]+"/"+map[bool]string{true: "old-manifest", false: "new-manifest"}[old], func(t *testing.T) {
				source, archive := managedBackupFixture(t, false)
				if old {
					archive = withoutRelocationMetadata(t, archive)
				}
				target := source
				if !same {
					target = t.TempDir()
					writeTestFile(t, filepath.Join(target, "prior-target"), "keep")
				}
				result, err := RestoreContents(archive, target, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					cfg, err := config.Load(filepath.Join(target, "config.yaml"))
					if err != nil {
						t.Fatal(err)
					}
					st, err := state.Open(filepath.Join(target, "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					m := driverrepo.New(cfg.DeviceRepository, target, st)
					active, err := st.ActiveDriverRepoInstall("drivers/meter.lua")
					if err != nil {
						st.Close()
						t.Fatalf("restore lost the chosen driver at start %d: %v", i, err)
					}
					want := filepath.Join(target, "custom-repository", "installed", "2.0.0", "meter.lua")
					prev := filepath.Join(target, "custom-repository", "installed", "1.0.0", "meter.lua")
					if active.InstalledPath != want || active.PreviousInstalledPath != prev || active.Version != "2.0.0" || active.RepositoryFormat != "lua-driver" || active.FTWSigned {
						st.Close()
						t.Fatalf("selection metadata changed: %+v", active)
					}
					history, err := st.DriverRepoInstallsByDriver("migration-meter")
					if err != nil || len(history) != 2 {
						st.Close()
						t.Fatalf("version history: %v %v", history, err)
					}
					for _, row := range history {
						if !strings.HasPrefix(row.InstalledPath, filepath.Join(target, "custom-repository")+string(filepath.Separator)) {
							st.Close()
							t.Fatalf("old history still refers to source: %+v", row)
						}
					}
					path, err := filepath.EvalSymlinks(filepath.Join(m.EffectiveDir(), "meter.lua"))
					resolvedWant, _ := filepath.EvalSymlinks(want)
					if err != nil || path != resolvedWant {
						st.Close()
						t.Fatalf("effective driver: %s %v", path, err)
					}
					st.Close()
				}
				if _, err := RevertContents(target, result.SafetyDir); err != nil {
					t.Fatal(err)
				}
				if !same {
					b, _ := os.ReadFile(filepath.Join(target, "prior-target"))
					if string(b) != "keep" {
						t.Fatal("return lost prior target")
					}
				}
			})
		}
	}
}

func TestRestoreRejectsExternalDriverUndoPathBeforeActivation(t *testing.T) {
	_, archive := managedBackupFixture(t, true)
	target := t.TempDir()
	writeTestFile(t, filepath.Join(target, "prior-target"), "keep")
	if _, err := RestoreContents(archive, target, time.Now()); err == nil || !strings.Contains(err.Error(), "driver repository") {
		t.Fatalf("external driver path accepted: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(target, "prior-target"))
	if string(b) != "keep" {
		t.Fatal("failed restore replaced target")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 1 {
		t.Fatal("failed restore left staging")
	}
}
