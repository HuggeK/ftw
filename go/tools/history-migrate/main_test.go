package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/srcfl/ftw/go/internal/state"
	_ "modernc.org/sqlite"
)

func TestRealBetaRetainedParquetMergesDistinctReadings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cfg, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO config VALUES('history_duckdb_generation','cold-days'),('saved_goal','80% by 07:00')`)
	cfg.Close()
	if err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, stmt := range betaSchema {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Exec(`DROP TABLE ts_series_hour;INSERT INTO history_migrations(name) VALUES('generation:cold-days');INSERT INTO ts_drivers VALUES(7,'meter');INSERT INTO ts_metrics VALUES(9,'power','W');INSERT INTO ts_samples SELECT 7,9,1800000000000+i,i/10.0 FROM range(1,5004) t(i);CHECKPOINT`); err != nil {
		t.Fatal(err)
	}
	type coldRow struct {
		Ts     int64   `parquet:"ts_ms"`
		Driver string  `parquet:"driver"`
		Metric string  `parquet:"metric"`
		Value  float64 `parquet:"value"`
	}
	cold := filepath.Join(dir, "cold")
	day := time.UnixMilli(1800000000000).UTC().Format("2006/01/02.parquet")
	archive := filepath.Join(cold, day)
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := parquet.NewGenericWriter[coldRow](f)
	_, err = w.Write([]coldRow{{1800000000001, "meter", "power", 999}, {1800000006000, "meter", "power", 777}, {1800000002000, "new-meter", "power", 888}})
	if err = errors.Join(err, w.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, nil, cold); err != nil {
		t.Fatal(err)
	}
	previous := state.RetireRawOnOpen
	state.RetireRawOnOpen = true
	t.Cleanup(func() { state.RetireRawOnOpen = previous })
	for start := 0; start < 2; start++ {
		s, err := state.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AbsorbColdHistory(context.Background(), cold); err != nil {
			s.Close()
			t.Fatal(err)
		}
		for _, points := range []int{0, 1000} {
			got, err := s.LoadSeriesBucketsOrRaw("meter", "power", 1800000000000, 1800000010000, points)
			var n int64
			var sum float64
			for _, p := range got {
				n += p.N
				sum += p.V * float64(p.N)
			}
			want := float64(5003*5004)/20 + 777
			if err != nil || n != 5004 || math.Abs(sum-want) > 1e-6 {
				s.Close()
				t.Fatalf("start=%d points=%d n=%d sum=%v want=%v err=%v", start, points, n, sum, want, err)
			}
			newSeries, err := s.LoadSeriesBucketsOrRaw("new-meter", "power", 1800000000000, 1800000010000, points)
			if err != nil || len(newSeries) != 1 || newSeries[0].N != 1 || newSeries[0].V != 888 {
				s.Close()
				t.Fatal("new series lost", newSeries, err)
			}
		}
		if goal, found := s.LoadConfig("saved_goal"); !found || goal != "80% by 07:00" {
			s.Close()
			t.Fatal("saved goal changed", goal, found)
		}
		s.Close()
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("verified duplicate file still active", err)
	}
}

func TestRealBetaWithoutHourTableSurvivesRawRetirement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cfg, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO config VALUES('history_duckdb_generation','no-hours')`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	source, err := sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, stmt := range betaSchema {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Exec(`DROP TABLE ts_series_hour;
 INSERT INTO history_migrations(name) VALUES('generation:no-hours');
 INSERT INTO ts_drivers VALUES(7,'offline-meter');
 INSERT INTO ts_metrics VALUES(9,'power','W');
 INSERT INTO ts_samples SELECT 7,9,1800000000000+i,i/10.0 FROM range(1,5004) t(i);
 CHECKPOINT`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = state.ConvertBetaHistory(ctx, path, source, func(phase string) {
		if strings.HasPrefix(phase, "summarize samples: ") {
			cancel()
		}
	})
	// Check retirement first so the released converter fails on the data
	// defect, rather than only on its missing resumable summary phase.
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("resume summaries", err)
	}
	previous := state.RetireRawOnOpen
	state.RetireRawOnOpen = true
	t.Cleanup(func() { state.RetireRawOnOpen = previous })
	for start := range 2 {
		s, err := state.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		points, readErr := s.LoadSeriesBucketsOrRaw("offline-meter", "power", 1800000000000, 1800000010000, 0)
		s.Close()
		want := float64(5003*5004) / (2 * 10 * 5003)
		if readErr != nil || len(points) != 1 || math.Abs(points[0].V-want) > 1e-9 || points[0].N != 5003 {
			t.Fatalf("start %d lost summary: points=%v err=%v want=%v", start, points, readErr, want)
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("summary phase did not stop after a committed batch", err)
	}
}

func TestRealBetaConversionPreservesIDsGoalsAndHotSamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cfg, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO config VALUES('history_duckdb_generation','test-generation'),('goal','80% by 07:00')`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	source, err := sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	for _, stmt := range betaSchema {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO history_migrations(name) VALUES('generation:test-generation'),('sqlite-v1')`,
		`INSERT INTO ts_drivers VALUES(7,'meter')`,
		`INSERT INTO ts_metrics VALUES(9,'power','W')`,
		`INSERT INTO ts_samples SELECT 7,9,i,i/10.0 FROM range(1,10001) t(i)`,
		// Leave physical row-ID holes; a resumed copy must not skip nearby data.
		`INSERT INTO ts_samples VALUES(7,9,20000,1)`,
		`DELETE FROM ts_samples WHERE ts_ms=20000`,
		`INSERT INTO ts_series_hour VALUES(7,9,1577836800000,84000,7000,7000,12,1577836860000)`,
		`INSERT INTO history_warm VALUES(1000,123.5,NULL,-12.25,100,0.8,'{"mode":"warm"}')`,
		`INSERT INTO history_cold VALUES(10,100,NULL,0,100,0.8,'{}')`,
		`INSERT INTO energy_daily VALUES('2026-09-01',1,2,3,4,5,6,1000)`,
		`INSERT INTO energy_assets VALUES('ev:stable','easee:stable','vehicle_charger','Car',0,1800000000000,1800000120000)`,
		`INSERT INTO energy_ledger_entries VALUES(1,'ev:stable','vehicle_charge',1800000000000,300000,200,'hardware_counter','measured','counter',2,1800000120000)`,
		`INSERT INTO energy_ledger_cursors VALUES('ev:stable','vehicle_charge','counter',300,1800000120000)`,
		`CHECKPOINT`,
	} {
		if _, err := source.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("FTW_STORAGE_ADMISSION") == "1" {
		for _, q := range []string{
			`INSERT INTO ts_metrics SELECT 1000+i,'metric-'||i,'W' FROM range(10) t(i)`,
			`INSERT INTO ts_samples SELECT 7,1000+(i%10),200000+(i//10),i/10.0 FROM range(1000000) t(i)`,
			`CHECKPOINT`,
		} {
			if _, err := source.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	hot, err := sql.Open("sqlite", filepath.Join(dir, state.HotHistoryFilename))
	if err != nil {
		t.Fatal(err)
	}
	_, err = hot.Exec(`CREATE TABLE history_hot(ts_ms INTEGER PRIMARY KEY,grid_w REAL,pv_w REAL,bat_w REAL,load_w REAL,bat_soc REAL,json TEXT);
 CREATE TABLE ts_drivers(id INTEGER PRIMARY KEY,name TEXT);
 CREATE TABLE ts_metrics(id INTEGER PRIMARY KEY,name TEXT,unit TEXT);
 CREATE TABLE ts_samples(driver_id INTEGER,metric_id INTEGER,ts_ms INTEGER,value REAL);
 CREATE TABLE hot_ticks(id TEXT,ts_ms INTEGER,payload TEXT);
 INSERT INTO ts_drivers VALUES(999,'meter');INSERT INTO ts_metrics VALUES(888,'power','W');
 INSERT INTO ts_samples VALUES(999,888,10001,1000.1),(999,888,10002,1000.2);
 INSERT INTO history_hot VALUES(10002,1000.2,0,0,0,0,'{}');`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		counter := float64(100 + i*100)
		payload, err := json.Marshal(struct{ Observations []state.EnergyObservation }{[]state.EnergyObservation{{AssetID: "ev:stable", DeviceID: "easee:stable", AssetKind: state.AssetVehicleCharger, Label: "Car", Flow: state.FlowVehicleCharge, AtMs: 1800000000000 + int64(i)*60000, CounterWh: &counter}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hot.Exec(`INSERT INTO hot_ticks VALUES(?,?,?)`, fmt.Sprint(i), 1800000000000+int64(i)*60000, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	hot.Close()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	source, err = sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?access_mode=read_only&threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	err = state.ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "copy and verify ts_samples" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected partial copy", err)
	}
	// Resume a committed prefix from the released 1024-row converter. The
	// cursor is deliberately not aligned to the new numeric batch size.
	partial, err := sql.Open("sqlite", state.HistoryDatabasePath(path)+".converting")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := partial.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Released partial copies also have the incrementally maintained index.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_ts_samples_ts ON ts_samples(ts_ms)`); err != nil {
		t.Fatal(err)
	}
	rows, err := source.Query(`SELECT driver_id,metric_id,ts_ms,value FROM ts_samples WHERE rowid<1024`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var driver, metric, ts int64
		var value float64
		if err := rows.Scan(&driver, &metric, &ts, &value); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO ts_samples VALUES(?,?,?,?)`, driver, metric, ts, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO conversion_progress VALUES('ts_samples:physical','1024')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, func(s string) { t.Log(s) }); err != nil {
		t.Fatal(err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("retry", err)
	}
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _ := s.LoadConfig("goal"); got != "80% by 07:00" {
		t.Fatal(got)
	}
	db, err := sql.Open("sqlite", state.HistoryDatabasePath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err = db.Query(`SELECT ts_ms,value FROM ts_samples WHERE driver_id=7 AND metric_id=9 ORDER BY ts_ms`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var ts int64
		var value float64
		if err := rows.Scan(&ts, &value); err != nil {
			t.Fatal(err)
		}
		n++
		if ts != int64(n) || value != float64(n)/10 {
			t.Fatalf("changed raw row %d: %d/%v", n, ts, value)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || n != 10002 {
		t.Fatalf("raw rows=%d err=%v", n, err)
	}
	var indexed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_ts_samples_ts'`).Scan(&indexed); err != nil || indexed != 1 {
		t.Fatal("published history lacks its time index", indexed, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM ts_drivers WHERE name='meter'`).Scan(&id); err != nil || id != 7 {
		t.Fatal("changed identifier", id, err)
	}
	if os.Getenv("FTW_STORAGE_ADMISSION") == "1" {
		var count int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM ts_samples WHERE metric_id>=1000`).Scan(&count); err != nil || count != 1000000 {
			t.Fatal("large interleaved copy differs", count, err)
		}
	}
	var summaryCount int64
	if err := db.QueryRow(`SELECT n FROM ts_series_hour WHERE hour_ms=1577836800000`).Scan(&summaryCount); err != nil || summaryCount != 12 {
		t.Fatal("old summary lost", summaryCount, err)
	}
	if err := db.QueryRow(`SELECT n FROM ts_series_hour WHERE hour_ms=0`).Scan(&summaryCount); err != nil || summaryCount != 10002 {
		t.Fatal("hot sample summary lost", summaryCount, err)
	}
	if os.Getenv("FTW_STORAGE_ADMISSION") == "1" {
		if err := db.QueryRow(`SELECT SUM(n) FROM ts_buckets WHERE metric_id>=1000 AND resolution_ms=10000`).Scan(&summaryCount); err != nil || summaryCount != 1000000 {
			t.Fatal("large dataset lacks complete chart summaries", summaryCount, err)
		}
	}
	var energy float64
	if err := db.QueryRow(`SELECT SUM(energy_wh) FROM energy_ledger_entries WHERE asset_id='ev:stable'`).Scan(&energy); err != nil || energy != 300 {
		t.Fatal("hot counter replay changed energy", energy, err)
	}
	var jsonText string
	if err := db.QueryRow(`SELECT json FROM history_warm WHERE ts_ms=1000`).Scan(&jsonText); err != nil || jsonText != `{"mode":"warm"}` {
		t.Fatal("snapshot changed", jsonText, err)
	}
	for _, name := range []string{"history.duckdb", state.HotHistoryFilename} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(fmt.Errorf("original removed: %s: %w", name, err))
		}
	}
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	bytes := int64(usage.Maxrss) * 1024
	if runtime.GOOS == "darwin" {
		bytes = int64(usage.Maxrss)
	}
	t.Logf("kernel peak RSS: %.2f MiB", float64(bytes)/(1<<20))
	if bytes > 256<<20 {
		t.Fatalf("converter peak exceeds 256 MiB: %d bytes", bytes)
	}

}
