package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

type relocationSample struct {
	TsMs   int64   `parquet:"ts_ms"`
	Driver string  `parquet:"driver"`
	Metric string  `parquet:"metric"`
	Value  float64 `parquet:"value"`
}

func relocationFixture(t *testing.T, stored bool, external string) (string, string) {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(data, "cold", "2026", "01"), 0700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(data, "state.db")
	seed := filepath.Join(data, "config.yaml")
	cold := filepath.Join(data, "cold")
	if external != "" {
		cold = external
	}
	yaml := "site:\n  name: Current site\nfuse:\n  max_amps: 16\nstate:\n  path: " + database + "\n  cold_dir: " + cold + "\n  backup_dir: " + filepath.Join(data, "backups") + "\ndrivers:\n  - name: meter\n    is_site_meter: true\n    lua: " + filepath.Join(data, "meter.lua") + "\n    capabilities:\n      standalone: true\n    config:\n      serial: unchanged\n"
	writeTestFile(t, seed, yaml)
	writeTestFile(t, filepath.Join(data, "meter.lua"), "DRIVER={id='meter'}")
	writeTestFile(t, filepath.Join(data, "nova.key"), "private fixture identity")
	rows := []relocationSample{{1, "meter", "power", 123}, {2, "meter", "power", 456}}
	if err := parquet.WriteFile(filepath.Join(data, "cold", "2026", "01", "01.parquet"), rows); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveConfig("ev_goal", "80% by 07:00"); err != nil {
		t.Fatal(err)
	}
	if stored {
		cfg, err := config.Load(seed)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err = config.InitializeStorage(seed, database, cfg, st)
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := st.Configuration()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(c.Document, &doc); err != nil {
			t.Fatal(err)
		}
		doc["future_setting"] = json.RawMessage(`{"counter":9007199254740993,"secret":"keep me"}`)
		doc["lan_password_hash"] = json.RawMessage(`"fixture hash"`)
		raw, _ := json.Marshal(doc)
		if _, err := st.SaveConfiguration(raw, c.Revision, nil); err != nil {
			t.Fatal(err)
		}
		// This stale YAML must not replace the active SQLite document.
		writeTestFile(t, seed, strings.ReplaceAll(yaml, "Current site", "Stale site"))
	}
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: database, DataDir: data, OutputDir: filepath.Join(root, "backup")})
	if err != nil {
		t.Fatal(err)
	}
	return data, info.Path
}

func TestRestoreRelocatesStoragePathsAndPreservesParquet(t *testing.T) {
	for _, stored := range []bool{false, true} {
		for _, oldManifest := range []bool{false, true} {
			t.Run(fmtName(stored, oldManifest), func(t *testing.T) {
				source, archive := relocationFixture(t, stored, "")
				if oldManifest {
					archive = withoutRelocationMetadata(t, archive)
				}
				before, _ := os.ReadFile(archive)
				target := filepath.Join(t.TempDir(), "target")
				if err := os.MkdirAll(target, 0700); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(target, "previous"), "target before restore")
				result, err := RestoreContents(archive, target, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					cfg, err := config.Load(filepath.Join(target, "config.yaml"))
					if err != nil {
						t.Fatal(err)
					}
					if cfg.State.Path != filepath.Join(target, "state.db") || cfg.State.ColdDir != filepath.Join(target, "cold") || cfg.State.BackupDir != filepath.Join(target, "backups") {
						t.Fatalf("paths still refer to the old directory: %+v", cfg.State)
					}
					if cfg.Site.Name != "Current site" || cfg.Drivers[0].Lua != filepath.Join(target, "meter.lua") || cfg.Drivers[0].Config["serial"] != "unchanged" {
						t.Fatalf("settings changed: %+v", cfg)
					}
					rows, err := parquet.ReadFile[relocationSample](filepath.Join(cfg.State.ColdDir, "2026", "01", "01.parquet"))
					if err != nil || len(rows) != 2 || rows[1].Value != 456 {
						t.Fatalf("Parquet unavailable at the active path: %v %v", rows, err)
					}
					st, err := state.Open(filepath.Join(target, "state.db"))
					if err != nil {
						t.Fatal(err)
					}
					goal, _ := st.LoadConfig("ev_goal")
					st.Close()
					if goal != "80% by 07:00" {
						t.Fatal("goal changed")
					}
					if stored {
						c, err := state.ReadConfiguration(filepath.Join(target, "state.db"))
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Contains(c.Document, []byte(`9007199254740993`)) || !bytes.Contains(c.Document, []byte(`"secret":"keep me"`)) || !bytes.Contains(c.Document, []byte(`"lan_password_hash":"fixture hash"`)) {
							t.Fatal("unknown setting or credential changed")
						}
					}
				}
				if _, err := RevertContents(target, result.SafetyDir); err != nil {
					t.Fatal(err)
				}
				if b, _ := os.ReadFile(filepath.Join(target, "previous")); string(b) != "target before restore" {
					t.Fatal("revert lost previous data")
				}
				after, _ := os.ReadFile(archive)
				if !bytes.Equal(before, after) {
					t.Fatal("restore changed the archive")
				}
				if b, _ := os.ReadFile(filepath.Join(source, "nova.key")); string(b) != "private fixture identity" {
					t.Fatal("source identity changed")
				}
			})
		}
	}
}

func fmtName(stored, old bool) string {
	return map[bool]string{true: "sqlite", false: "yaml"}[stored] + "/" + map[bool]string{true: "old-manifest", false: "new-manifest"}[old]
}

func TestRestoreRejectsExternalColdPathBeforeReplacingTarget(t *testing.T) {
	_, archive := relocationFixture(t, true, "/external/archive")
	target := t.TempDir()
	writeTestFile(t, filepath.Join(target, "untouched"), "previous data")
	if _, err := RestoreContents(archive, target, time.Now()); err == nil || !strings.Contains(err.Error(), "state.cold_dir") {
		t.Fatalf("external archive was silently relocated: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "untouched")); string(b) != "previous data" {
		t.Fatal("failed restore replaced target")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 1 {
		t.Fatal("failed restore left staging or activation entries")
	}
}

func TestRestoreAtSamePathKeepsExternalColdMount(t *testing.T) {
	source, archive := relocationFixture(t, true, "/external/archive")
	if _, err := RestoreContents(archive, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(source, "config.yaml"))
	if err != nil || cfg.State.ColdDir != "/external/archive" {
		t.Fatalf("same-path recovery changed external mount: %v %v", cfg, err)
	}
}

func TestRestoreUsesRecordedNestedSeed(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(data, "settings"), 0700); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(data, "settings", "site.yaml")
	database := filepath.Join(data, "state.db")
	writeTestFile(t, seed, "site:\n  name: Nested seed\nfuse:\n  max_amps: 16\nstate:\n  path: "+database+"\n  cold_dir: "+filepath.Join(data, "cold")+"\n")
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
	archive, err := Create(context.Background(), CreateOptions{State: st, StatePath: database, DataDir: data, ConfigPath: seed, OutputDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if _, err := restoreFresh(archive.Path, target, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(filepath.Join(target, "settings", "site.yaml"))
	if err != nil || loaded.ConfigDatabase != filepath.Join(target, "state.db") || loaded.State.ColdDir != filepath.Join(target, "cold") {
		t.Fatalf("nested locator was not moved: %v %v", loaded, err)
	}
}

// Old schema-1 manifests predate source_data_dir and config_file.
func withoutRelocationMetadata(t *testing.T, archive string) string {
	t.Helper()
	src, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	zr, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), "old.ftwbak")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == manifestPath {
			var m map[string]json.RawMessage
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatal(err)
			}
			delete(m, "source_data_dir")
			delete(m, "config_file")
			b, _ := json.Marshal(m)
			h.Size = int64(len(b))
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(b); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(tw, tr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}
