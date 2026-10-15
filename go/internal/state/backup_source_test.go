package state

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBackupSourceCopiesWithoutRunningMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_only(value TEXT); INSERT INTO legacy_only VALUES ('preserved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.db.gz")
	if err := source.BackupToCompressed(dst); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !os.IsNotExist(err) {
		t.Fatalf("backup source created cache.db: %v", err)
	}
	if _, err := os.Stat(path + ".clean"); !os.IsNotExist(err) {
		t.Fatalf("backup source wrote clean marker: %v", err)
	}
}

// These tables did not exist in the published 3.8 split SQLite history.
var backupQualityTables = []string{
	"history_quality", "history_daily_quality", "history_dashboard_partial",
	"history_site_energy_partial", "history_partial_cursor", "energy_daily_partial",
}

func stoppedSplitBackupSource(t *testing.T, change func(string)) string {
	t.Helper()
	s := freshStore(t)
	if err := s.SaveConfig("selected-goal", "80%"); err != nil {
		t.Fatal(err)
	}
	p := HistoryPoint{TsMs: 1000, GridW: 1234, JSON: `{}`}
	if err := s.RecordTick(p, []Sample{{Driver: "meter", Metric: "grid_w", TsMs: 1000, Value: 1234}}); err != nil {
		t.Fatal(err)
	}
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	change(historyDatabasePath(path))
	return path
}

func TestOfflineBackupExportsLegacySplitHistoryWithoutQualityTables(t *testing.T) {
	path := stoppedSplitBackupSource(t, func(historyPath string) {
		db, err := openRaw(historyPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, table := range backupQualityTables {
			if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
		}
	})
	before := readBackupSourceBytes(t, path)
	source, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dst := filepath.Join(t.TempDir(), "legacy.db.gz")
	if err := source.BackupToCompressed(dst); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	assertBackupSourceBytes(t, path, before)

	in, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	z, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	raw := filepath.Join(t.TempDir(), "state.db")
	out, err := os.Create(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, z); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, ok := restored.LoadConfig("selected-goal"); !ok || got != "80%" {
		t.Fatalf("restored goal = %q, %v", got, ok)
	}
	points, err := restored.LoadHistory(1000, 1000, 0)
	if err != nil || len(points) != 1 || points[0].GridW != 1234 || points[0].Missing != 0 {
		t.Fatalf("restored history = %+v, %v", points, err)
	}
	samples, err := restored.LoadSeries("meter", "grid_w", 1000, 1000, 0)
	if err != nil || len(samples) != 1 || samples[0].Value != 1234 {
		t.Fatalf("restored samples = %+v, %v", samples, err)
	}
}

func TestOfflineBackupRefusesIncompleteSplitHistory(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
	}{
		{"quality", `DROP TABLE history_quality`, "incomplete history quality tables"},
		{"required", `DROP TABLE ts_samples`, "ts_samples"},
		{"view", `DROP TABLE history_quality; CREATE VIEW history_quality AS SELECT 1`, "history_quality is not a table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stoppedSplitBackupSource(t, func(historyPath string) {
				db, err := openRaw(historyPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(tc.sql); err != nil {
					t.Fatal(err)
				}
			})
			before := readBackupSourceBytes(t, path)
			source, err := OpenBackupSource(path)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			dst := filepath.Join(t.TempDir(), "refused.db.gz")
			if err := source.BackupToCompressed(dst); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backup error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("published a refused backup: %v", err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			assertBackupSourceBytes(t, path, before)
		})
	}
}

func readBackupSourceBytes(t *testing.T, path string) [][]byte {
	t.Helper()
	var data [][]byte
	for _, p := range []string{path, historyDatabasePath(path)} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, b)
	}
	return data
}

func assertBackupSourceBytes(t *testing.T, path string, before [][]byte) {
	t.Helper()
	for i, after := range readBackupSourceBytes(t, path) {
		if !bytes.Equal(before[i], after) {
			t.Fatal("offline backup changed a source database")
		}
	}
}
