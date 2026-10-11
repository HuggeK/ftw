package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parquet-go/parquet-go"
)

func TestBetaColdDefaultUsesCoreWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path := filepath.Join(root, "nested", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO config VALUES('settings/config_v1','{"version":1,"revision":1,"document":{"config":{"state":{"path":"nested/state.db"}}}}')`); err != nil {
		t.Fatal(err)
	}
	got, err := betaConversionColdDir(context.Background(), db, path, nil)
	if err != nil || got != filepath.Join(root, "cold") {
		t.Fatal("default differs from Core working directory", got, err)
	}
}

func TestBetaColdScanKeepsKnownNonSampleArchives(t *testing.T) {
	cold := filepath.Join(t.TempDir(), "cold")
	files := map[string]string{"diagnostics/2026/06/14.parquet": "diagnostic bytes", "2026/06/14.buckets.parquet": "bucket bytes", "2026/06/14.legacy-buckets.parquet": "older bucket bytes"}
	for name, value := range files {
		path := filepath.Join(cold, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(cold, "2026", "06", "14.parquet")
	if err := writeParquetDay(path, []parquetSampleRow{{1781438400000, "meter", "power", 10}}); err != nil {
		t.Fatal(err)
	}
	got, err := conversionColdPaths(cold)
	if err != nil || len(got) != 1 || got[0] != path {
		t.Fatal("known non-sample files entered conversion", got, err)
	}
	for name, value := range files {
		data, err := os.ReadFile(filepath.Join(cold, name))
		if err != nil || string(data) != value {
			t.Fatal("non-sample file changed", name, err)
		}
	}
}

func TestBetaConversionColdOverrideKeepsAuthoritativeSettings(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	cold := filepath.Join(filepath.Dir(path), "copied-archive")
	archive := filepath.Join(cold, "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(archive, []parquetSampleRow{{2301, "meter", "power", 777}}); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"version": 1, "revision": 42, "document": map[string]any{"config": map[string]any{"state": map[string]any{"cold_dir": filepath.Join(filepath.Dir(path), "unmounted-old-archive")}, "private_secret": "unchanged", "unknown_field": "keep"}}}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Exec(`INSERT OR REPLACE INTO config VALUES(?,?)`, configurationKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	err = ConvertBetaHistory(context.Background(), path, source, nil)
	if err == nil || !strings.Contains(err.Error(), "cold directory is unavailable") {
		t.Fatal("missing authority silently ignored", err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil, cold); err != nil {
		t.Fatal("explicit stopped copy override", err)
	}
	cfg, err = sql.Open("sqlite", ReadOnlyDatabaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	var got string
	if err := cfg.QueryRow(`SELECT value FROM config WHERE key=?`, configurationKey).Scan(&got); err != nil || got != string(raw) {
		t.Fatal("override rewrote stored settings or secrets", err)
	}
}

func TestBetaConversionColdLateRowsKeepSummaryOnlyPrefix(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	old, err := sql.Open("sqlite", BetaHistoryDatabasePath(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`INSERT INTO ts_series_hour SELECT d.id,m.id,3600000,100,10,10,10,3600100 FROM ts_drivers d,ts_metrics m WHERE d.name='meter' AND m.name='power'`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	cold := filepath.Join(filepath.Dir(path), "cold")
	archive := filepath.Join(cold, "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	// File order is not an assurance that observations came in time order.
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := parquet.NewGenericWriter[parquetSampleRow](f)
	_, err = w.Write([]parquetSampleRow{{3600300, "meter", "power", 30}, {3600200, "meter", "power", 20}})
	if err = errors.Join(err, w.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal(err)
	}
	previous := RetireRawOnOpen
	RetireRawOnOpen = true
	t.Cleanup(func() { RetireRawOnOpen = previous })
	for start := 0; start < 2; start++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.coldDir = cold
		if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
			s.Close()
			t.Fatal(err)
		}
		for _, points := range []int{0, 1000} {
			got, err := s.LoadSeriesBucketsOrRaw("meter", "power", 3600000, 7199999, points)
			if err != nil || len(got) != 1 || got[0].N != 12 || got[0].V != 12.5 || got[0].Min != 10 || got[0].Max != 30 || got[0].ResolutionMS != seriesHourMs || got[0].FirstMS != 0 || got[0].Last != nil {
				s.Close()
				t.Fatalf("summary prefix lost or raw detail invented: %v %v", got, err)
			}
		}
		s.Close()
	}
}

func TestBetaConversionColdAmbiguousSummaryStopsBeforePublish(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	old, err := sql.Open("sqlite", BetaHistoryDatabasePath(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`INSERT INTO ts_series_hour SELECT d.id,m.id,3600000,100,10,10,10,3600100 FROM ts_drivers d,ts_metrics m WHERE d.name='meter' AND m.name='power'`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(filepath.Dir(path), "cold", "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(archive, []parquetSampleRow{{3600050, "meter", "power", 10}}); err != nil {
		t.Fatal(err)
	}
	before, err := historyFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	err = ConvertBetaHistory(context.Background(), path, source, nil)
	if err == nil || !strings.Contains(err.Error(), "without raw timestamps") {
		t.Fatal("guessed membership in expired raw detail", err)
	}
	if _, err := os.Stat(historyDatabasePath(path)); !os.IsNotExist(err) {
		t.Fatal("ambiguous data published", err)
	}
	after, err := historyFileHash(path)
	if err != nil || before != after {
		t.Fatal("failed conversion changed config", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("archive removed", err)
	}
}

func TestBetaConversionRefusesOldReceiptWithRetainedColdFile(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(filepath.Dir(path), "cold", "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(archive, []parquetSampleRow{{1, "meter", "power", 1}}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := historyFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := historyFileHash(historyDatabasePath(path))
	if err != nil {
		t.Fatal(err)
	}
	err = ConvertBetaHistory(context.Background(), path, source, nil)
	if err == nil || !strings.Contains(err.Error(), "unverified cold files") {
		t.Fatal("older incomplete conversion accepted", err)
	}
	stateAfter, e1 := historyFileHash(path)
	historyAfter, e2 := historyFileHash(historyDatabasePath(path))
	if e1 != nil || e2 != nil || stateBefore != stateAfter || historyBefore != historyAfter {
		t.Fatal("older destination changed", e1, e2)
	}
}

func TestBetaConversionMergesRetainedParquetBeforeRetirement(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	cold := filepath.Join(filepath.Dir(path), "cold")
	archive := filepath.Join(cold, "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	// A stale overlapping file value must not replace the selected beta value.
	// A late value and a new series must survive, even in the same hour.
	rows := []parquetSampleRow{{1, "meter", "power", 999}, {2301, "meter", "power", 777}, {1000, "other", "power", 99}}
	if err := writeParquetDay(archive, rows); err != nil {
		t.Fatal(err)
	}
	before, err := historyFileHash(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal(err)
	}
	if after, err := historyFileHash(archive); err != nil || after != before {
		t.Fatal("conversion changed the original archive", err)
	}
	previous := RetireRawOnOpen
	RetireRawOnOpen = true
	t.Cleanup(func() { RetireRawOnOpen = previous })
	for start := 0; start < 2; start++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.coldDir = cold
		if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
			s.Close()
			t.Fatal(err)
		}
		for _, points := range []int{0, 1000} {
			got, err := s.LoadSeriesBucketsOrRaw("meter", "power", 0, 3599999, points)
			var n int64
			var sum float64
			for _, p := range got {
				n += p.N
				sum += p.V * float64(p.N)
			}
			want := float64(2300*2301)/2 + 777
			if err != nil || n != 2301 || math.Abs(sum-want) > 1e-6 {
				s.Close()
				t.Fatalf("start=%d points=%d duplicate or missing rows: n=%d sum=%v want=%v err=%v", start, points, n, sum, want, err)
			}
			other, err := s.LoadSeriesBucketsOrRaw("other", "power", 0, 3599999, points)
			if err != nil || len(other) != 1 || other[0].N != 1 || other[0].V != 99 {
				s.Close()
				t.Fatalf("late series missing: %v %v", other, err)
			}
		}
		s.Close()
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("verified duplicate archive was not retired: %v", err)
	}
	// The beta database, including its authoritative value, remains available.
	var value float64
	if err := source.QueryRow(`SELECT value FROM ts_samples WHERE ts_ms=1`).Scan(&value); err != nil || value != 1 {
		t.Fatal("beta source changed", value, err)
	}
	if before == "" {
		t.Fatal("source file fingerprint missing")
	}
}

func TestBetaConversionRetainedParquetResumeRejectsChangedFile(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	t.Chdir(filepath.Dir(path))
	archive := filepath.Join(filepath.Dir(path), "cold", "1970", "01", "01.parquet")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	var rows []parquetSampleRow
	for i := 1; i <= 4097; i++ {
		rows = append(rows, parquetSampleRow{int64(i), "meter", "power", float64(i)})
	}
	if err := writeParquetDay(archive, rows); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := ConvertBetaHistory(ctx, path, source, func(phase string) {
		if strings.HasPrefix(phase, "cold samples: ") {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cold copy did not stop after a committed batch", err)
	}
	before, err := historyFileHash(archive)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Value = 999
	if err := writeParquetDay(archive, rows); err != nil {
		t.Fatal(err)
	}
	err = ConvertBetaHistory(context.Background(), path, source, nil)
	if err == nil || !strings.Contains(err.Error(), "source changed since") {
		t.Fatal("changed cold source accepted", err)
	}
	rows[0].Value = 1
	if err := writeParquetDay(archive, rows); err != nil {
		t.Fatal(err)
	}
	after, err := historyFileHash(archive)
	if err != nil || before != after {
		t.Fatal("fixture fingerprint not restored", err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("cold resume", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int64
	if err := s.history.QueryRow(`SELECT SUM(n) FROM ts_series_hour`).Scan(&n); err != nil || n != 4097 {
		t.Fatal("resume double counted cold rows", n, err)
	}
}
