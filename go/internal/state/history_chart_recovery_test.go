package state

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func retiredChartsFixture(t *testing.T, n int) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.history.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := tx.Exec(`INSERT INTO history_hot VALUES(?,7,11,0,18,0.5,'old')`, i); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"history_warm", "history_cold"} {
		if _, err := tx.Exec(`INSERT INTO ` + table + ` VALUES(0,13,17,0,30,0.6,'old-tier')`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	history := pathDirHistory(path)
	bytes, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	retired := history + ".raw-retired"
	if err := os.WriteFile(retired, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", history)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"history_hot", "history_warm", "history_cold"} {
		if _, err := db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO history_dashboard VALUES(10000,10000,10000,10000,1,19,23,0,42,0.7,'new',X'');
 INSERT INTO energy_ledger_entries VALUES(1,'site','import',3600000,3600000,12.5,'hardware_counter','measured','counter',1,3600000)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	RetireRawOnOpen = true
	t.Cleanup(func() { RetireRawOnOpen = false })
	return path, retired
}

func TestRecoverRetiredChartsKeepsNewHistoryAndSource(t *testing.T) {
	path, retired := retiredChartsFixture(t, 3)
	before, err := os.ReadFile(retired)
	if err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadHistory(0, 10000, 0)
		if err != nil || len(got) != 5 || got[4].GridW != 19 {
			s.Close()
			t.Fatalf("old and new charts: %+v %v", got, err)
		}
		var wh float64
		if err := s.history.QueryRow(`SELECT SUM(energy_wh) FROM energy_ledger_entries`).Scan(&wh); err != nil || wh != 12.5 {
			s.Close()
			t.Fatalf("changed ledger: %v %v", wh, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(retired)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("changed retained source", err)
	}
}

func TestRecoverChartsAfterRetiredFileCleanup(t *testing.T) {
	path, retired := retiredChartsFixture(t, 3)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO history_hot VALUES(1,7,11,0,18,0.5,'old');
 INSERT INTO history_warm VALUES(2,13,17,0,30,0.6,'warm');
 INSERT INTO history_cold VALUES(3,19,23,0,42,0.7,'cold')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(retired); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.LoadHistory(0, 10000, 0)
		if err != nil || len(got) != 4 || got[3].GridW != 19 {
			s.Close()
			t.Fatalf("bound source and new charts: %+v %v", got, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoverChartsAfterSourceTablesRetired(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(map[bool]string{false: "no retained file", true: "retained file"}[retain], func(t *testing.T) {
			path, retired := retiredChartsFixture(t, 3)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DROP TABLE history_hot; DROP TABLE history_warm; DROP TABLE history_cold;
 INSERT INTO config(key,value) VALUES('history_legacy_sources_retired','1')`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if !retain {
				if err := os.Remove(retired); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.LoadHistory(0, 10000, 0)
			want := 1
			if retain {
				want = 5
			}
			if err != nil || len(got) != want || got[len(got)-1].GridW != 19 {
				t.Fatalf("source tables retired: %+v %v", got, err)
			}
		})
	}
}

func TestRecoverRetiredChartsResumesAfterFailedChunk(t *testing.T) {
	path, _ := retiredChartsFixture(t, 2300)
	db, err := sql.Open("sqlite", pathDirHistory(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_recovery BEFORE INSERT ON history_hot WHEN NEW.ts_ms=1025 BEGIN SELECT RAISE(ABORT,'write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("reported complete recovery after a failed write")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&count); err != nil || count != 1024 {
		t.Fatalf("verified first chunk: %d %v", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_recovery`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&count); err != nil || count != 2300 {
		t.Fatalf("retry lost or repeated rows: %d %v", count, err)
	}
}

func TestRecoverRetiredChartsRejectsConflictingCurrentPoint(t *testing.T) {
	path, _ := retiredChartsFixture(t, 3)
	db, err := sql.Open("sqlite", pathDirHistory(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO history_hot VALUES(1,99,11,0,18,0.5,'newer')`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("overwrote a conflicting current point")
	} else if !strings.Contains(err.Error(), "differs from retained source") {
		t.Fatal(err)
	}
	var power float64
	if err := db.QueryRow(`SELECT grid_w FROM history_hot WHERE ts_ms=1`).Scan(&power); err != nil || power != 99 {
		t.Fatal("changed current data", power, err)
	}
}

func TestRecoverRetiredChartsDoesNotMixGenerations(t *testing.T) {
	path, retired := retiredChartsFixture(t, 3)
	db, err := sql.Open("sqlite", retired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM history_migrations WHERE name LIKE 'generation:%'; INSERT INTO history_migrations(name) VALUES('generation:unrelated')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&count); err != nil || count != 0 {
		t.Fatal("mixed unrelated history", count, err)
	}
}
