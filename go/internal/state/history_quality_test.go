package state

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPartialHistoryRefusesStorageThatCannotKeepQuality(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := HistoryPoint{TsMs: time.Now().UnixMilli(), Missing: MissingBattery, JSON: `{}`}
	for _, err := range []error{s.RecordHistory(p), s.BulkRecordHistory([]HistoryPoint{p}), s.EnqueueTelemetryTick(&p, nil, nil)} {
		if err == nil {
			t.Fatal("storage accepted unknown power without durable quality")
		}
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestPartialHistorySurvivesRecoveryRetentionBackupAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour)
	ms := base.UnixMilli()
	for i, offset := range []int64{1000, 2000, 5000, 10000, 20000, 40000, 61000} {
		p := HistoryPoint{TsMs: ms + offset, GridW: 1200, PVW: -300, BatW: 200, LoadW: 1300, BatSoC: .5, JSON: `{}`}
		if i >= 1 && i <= 3 {
			p.Missing = MissingBattery | MissingLoad | MissingSoC
			p.ClearMissingValues()
		}
		dashboardTick(t, s, fmt.Sprint(i), &p)
	}
	check := func(st *Store) {
		t.Helper()
		points, err := st.LoadHistory(ms, ms+120000, 1)
		if err != nil || len(points) != 1 {
			t.Fatal(points, err)
		}
		p := points[0]
		if p.GridW != 1200 || p.PVW != -300 || p.Missing != MissingBattery|MissingLoad|MissingSoC {
			t.Fatal(p)
		}
		d, err := st.DailyEnergy(ms, ms+120000)
		if err != nil {
			t.Fatal(err)
		}
		near(t, "independent grid import", d.ImportWh, 20)
		near(t, "independent PV generation", d.PVWh, 5)
		if d.Missing&MissingLoad == 0 || d.Missing&MissingGrid != 0 {
			t.Fatal(d)
		}
		// The recovery endpoint must not fill the preceding offline interval.
		near(t, "known battery charge", d.BatChargedWh, float64(200*(21000+20000))/3600000)
		if err := st.SaveDailyEnergy("2026-10-12", d); err != nil {
			t.Fatal(err)
		}
		cached, ok, err := st.LoadDailyEnergy("2026-10-12")
		if err != nil || !ok || cached.Missing != d.Missing || cached.Intervals != d.Intervals {
			t.Fatal(cached, ok, err)
		}
	}
	check(s)
	for _, age := range []time.Duration{48 * time.Hour, 40 * 24 * time.Hour, 732 * 24 * time.Hour} {
		if err := s.maintainDashboard(context.Background(), base.Add(age)); err != nil {
			t.Fatal(err)
		}
		// Chart retention ends at two years, energy quality keeps its quarter-hour evidence.
		if age < AggregateRetention {
			check(s)
		} else {
			d, err := s.DailyEnergy(ms, ms+120000)
			if err != nil || d.Missing&MissingLoad == 0 || d.ImportWh != 20 {
				t.Fatal(d, err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.DailyEnergy(ms, ms+120000)
	if err != nil || d.Missing&MissingLoad == 0 {
		t.Fatal(d, err)
	}
	dst := filepath.Join(t.TempDir(), "backup.gz")
	if err := s.BackupToCompressed(dst); err != nil {
		t.Fatal(err)
	}
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
	restoredPath := filepath.Join(t.TempDir(), "state.db")
	out, err := os.Create(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, z); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.DailyEnergy(ms, ms+120000)
	if err != nil || got.Missing != d.Missing || got.ImportWh != d.ImportWh {
		t.Fatal(got, err)
	}
}

func TestPartialHistoryKeepsSchema7ReadersSafeAcrossReturn(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute).UnixMilli()
	full := HistoryPoint{TsMs: base, GridW: 1200, BatW: 200, LoadW: 1000, BatSoC: .5, JSON: `{}`}
	dashboardTick(t, s, "full", &full)
	gap := HistoryPoint{TsMs: base + 1000, GridW: 1200, Missing: MissingBattery | MissingLoad | MissingSoC, JSON: `{}`}
	dashboardTick(t, s, "gap", &gap)
	var legacyRows, partialRows int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_dashboard`).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_dashboard_partial`).Scan(&partialRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 1 || partialRows != 1 {
		t.Fatal(legacyRows, partialRows)
	}
	var last int64
	if err := s.history.QueryRow(`SELECT value FROM history_site_cursor WHERE key='last'`).Scan(&last); err != nil || last != 0 {
		t.Fatal(last, err)
	}
	// An older Core resumes only complete observations in the original tables.
	// The new cursor must follow that later writer without counting its interval twice.
	legacyAt := base + 2000
	if _, err := s.history.Exec(`UPDATE history_site_cursor SET value=? WHERE key='last'`, legacyAt); err != nil {
		t.Fatal(err)
	}
	full.TsMs = base + 3000
	dashboardTick(t, s, "return", &full)
	d, err := s.DailyEnergy(base, base+4000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "known import before/after old writer", d.ImportWh, float64(2400000)/3600000)
	if d.Missing&MissingLoad == 0 {
		t.Fatal(d)
	}
	if SchemaVersion != 7 {
		t.Fatal("native update requires schema 7", SchemaVersion)
	}
}
