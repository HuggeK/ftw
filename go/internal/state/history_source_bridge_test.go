package state

import (
	"context"
	"testing"
	"time"
)

// Two hours of a 1000 W house, sampled every 5 s, with one disturbance one
// hour in. Observed on v0.154.0-beta.1 (19 October 2026): each short
// disturbance made the whole day's Home use unknown.
func TestSourceLoadDayForgivesShortGaps(t *testing.T) {
	const tick, hour, minute = int64(5000), int64(3600000), int64(60000)
	for _, c := range []struct {
		name string
		// stopMS is a process stop starting one hour in.
		stopMS int64
		// receipt alters the snapshot off ms after the first hour.
		receipt func(off int64, v *HistorySources)
		// Zero Wh means the total must be unknown.
		loadWh, importWh float64
	}{
		{name: "no disturbance", loadWh: 2000, importWh: 2000},
		{name: "20 s restart", stopMS: 20000, loadWh: 2000, importWh: 2000},
		{name: "20 s restart, EV and its device back after 30 s", stopMS: 20000, receipt: func(off int64, v *HistorySources) {
			if off <= 50000 {
				v.Missing, v.Topology = MissingEV, "grid"
			}
		}, loadWh: 2000, importWh: 2000},
		{name: "EV unknown 15 s", receipt: unknownFor(MissingEV, 15000), loadWh: 2000, importWh: 2000},
		// The unknown charger holds its last value; the meter keeps its own
		// readings. The household carries the 2 kW for 15 s, nothing else.
		{name: "EV starts while unknown", receipt: func(off int64, v *HistorySources) {
			if off > 0 {
				v.GridW, v.EVW = 3000, 2000
			}
			if off > 0 && off <= 15000 {
				v.Missing, v.EVW = MissingEV, 0
			}
		}, loadWh: 2000 + 2000*15.0/3600, importWh: 1000 + 1000*5.0/3600 + 3000*3595.0/3600},
		{name: "PV unknown 145 s", receipt: unknownFor(MissingPV, 145000), loadWh: 2000, importWh: 2000},
		{name: "EV unknown 30 min", receipt: unknownFor(MissingEV, 30*minute), loadWh: 2000, importWh: 2000},
		// A 2 kW charge ends 25 minutes into a 30-minute gap. The held 2 kW
		// would make the house negative, so it is not held from then; Home
		// use loses the six minutes this touches instead of 2 kW for five.
		{name: "EV finishes while unknown", receipt: func(off int64, v *HistorySources) {
			if off < 25*minute {
				v.GridW, v.EVW = 3000, 2000
			}
			if off > 0 && off <= 30*minute {
				v.Missing, v.EVW = MissingEV, 0
			}
		}, loadWh: 2000 - 1000*6.0/60, importWh: 1000 + 3000*25.0/60 + 1000*35.0/60},
		{name: "meter replaced", receipt: func(off int64, v *HistorySources) {
			if off > 0 {
				v.Topology = "new meter+charger"
			}
		}, loadWh: 2000, importWh: 2000},
		// The meter holds for two minutes. The other 185 s count as nothing,
		// well inside a tenth of the range; Home use loses the four whole
		// minutes they touch.
		{name: "meter unknown 5 min", receipt: unknownFor(MissingGrid, 5*minute), loadWh: 2000 - 1000*240.0/3600, importWh: 2000 - 1000*185.0/3600},
		{name: "3 min restart", stopMS: 3 * minute, loadWh: 1950, importWh: 1950},
		// Over a tenth of the range unrecorded: Home use is unknown, and
		// the recorded import stays known, as before.
		{name: "15 min restart", stopMS: 15 * minute, importWh: 1750},
		{name: "meter unknown 20 min", receipt: unknownFor(MissingGrid, 20*minute)},
		{name: "invalid configuration 30 min", receipt: unknownFor(MissingLoad, 30*minute), importWh: 2000},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := sourceStore(t)
			base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 24*hour
			for at := base; at <= base+2*hour; at += tick {
				off := at - base - hour
				if c.stopMS > 0 && off > 0 && off < c.stopMS {
					continue
				}
				v := HistorySources{GridW: 1000}
				if c.receipt != nil && off >= 0 {
					c.receipt(off, &v)
				}
				sourceTick(t, s, at, v)
				if c.stopMS > 0 && off == 0 {
					s = reopenSourceStore(t, s)
				}
			}
			d, err := s.DailyEnergy(base, base+2*hour)
			if err != nil {
				t.Fatal(err)
			}
			check := func(name string, flag HistoryMissing, got, want float64) {
				t.Helper()
				if known := d.Missing&flag == 0; known != (want != 0) {
					t.Fatalf("%s known=%v, want %v", name, known, want != 0)
				}
				if want != 0 && (got-want > 1e-6 || want-got > 1e-6) {
					t.Fatalf("%s %v Wh, want %v", name, got, want)
				}
			}
			check("Home use", MissingLoad, d.LoadWh, c.loadWh)
			check("import", MissingGrid, d.ImportWh, c.importWh)
			if q := d.Missing.Quality(); c.loadWh != 0 && q != "estimated" {
				t.Fatalf("quality %s, want estimated", q)
			}
			if d.Missing&MissingPV != 0 {
				t.Fatal("PV total became unknown")
			}
		})
	}
}

// A held value the balance rejects stays rejected until its source reports
// again. A later rise in house use must not revive it as a phantom charge.
func TestSourceLoadForgetsARejectedHold(t *testing.T) {
	const minute = int64(60000)
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 24*3600000
	for at := base; at <= base+60*minute; at += 5000 {
		off := at - base
		// A 1 kW house and a 2 kW charge. The charger falls silent at 10
		// minutes, the car finishes at 20, a heater adds 3 kW at 30, and the
		// charger reports again, at 0 W, after 50.
		v := HistorySources{GridW: 3000, EVW: 2000}
		switch {
		case off >= 30*minute:
			v.GridW, v.EVW = 4000, 0
		case off >= 20*minute:
			v.GridW, v.EVW = 1000, 0
		}
		if off > 10*minute && off <= 50*minute {
			v.Missing, v.EVW = MissingEV, 0
		}
		sourceTick(t, s, at, v)
	}
	var phantomWh float64
	var unknownMinutes int
	if err := s.history.QueryRow(`SELECT COALESCE(SUM(e.ev_charge_wh),0),COALESCE(SUM(COALESCE(q.missing,0)&?<>0),0) FROM history_source_energy e
 LEFT JOIN history_quality q ON q.kind='source_energy' AND q.start_ms=e.start_ms AND q.resolution_ms=e.resolution_ms
 WHERE e.start_ms>=? AND e.start_ms<?`, MissingEV, base+21*minute, base+50*minute).Scan(&phantomWh, &unknownMinutes); err != nil {
		t.Fatal(err)
	}
	if phantomWh != 0 || unknownMinutes != 29 {
		t.Fatalf("rejected charge revived: %v Wh charged, %d of 29 minutes unknown", phantomWh, unknownMinutes)
	}
}

// Training reads only measured minutes, so every minute the display holds
// or bridges keeps its marker, also after retention merges minutes.
func TestSourceLoadMarksEveryEstimatedMinute(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 24*3600000
	for at := base; at <= base+30*60000; at += 5000 {
		v := HistorySources{GridW: 1000}
		if at > base+10*60000 && at <= base+10*60000+15000 {
			v.Missing = MissingEV
		}
		sourceTick(t, s, at, v)
	}
	marked := func(width int64) []int64 {
		t.Helper()
		rows, err := s.history.Query(`SELECT start_ms FROM history_quality WHERE kind='source_energy' AND resolution_ms=? AND missing&?<>0 ORDER BY start_ms`, width, EstimatedLoad)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var start int64
			if err := rows.Scan(&start); err != nil {
				t.Fatal(err)
			}
			out = append(out, start-base)
		}
		return out
	}
	if got := marked(ArchiveResolutionMS); len(got) != 1 || got[0] != 10*60000 {
		t.Fatal("estimated minutes", got)
	}
	if err := s.rollSiteEnergy(context.Background(), base+30*60000); err != nil {
		t.Fatal(err)
	}
	if got := marked(15 * 60000); len(got) != 1 || got[0] != 0 {
		t.Fatal("marker lost when minutes merged", got)
	}
}

// Closed days cached under the whole-day rule are recomputed once.
func TestSourceDayCacheRecomputedOnce(t *testing.T) {
	s := sourceStore(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	sourceTick(t, s, day.UnixMilli(), HistorySources{GridW: 1000})
	if _, err := s.history.Exec(`DELETE FROM history_migrations WHERE name=?`, sourceDayCoverageMigration); err != nil {
		t.Fatal(err)
	}
	key := day.Format("2006-01-02")
	save := func() {
		t.Helper()
		if err := s.SaveDailyEnergyForSourceMode(key, DayEnergy{Missing: MissingLoad, Intervals: 1}, true); err != nil {
			t.Fatal(err)
		}
	}
	cached := func() bool {
		t.Helper()
		_, ok, err := s.LoadDailyEnergy(key)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	save()
	s = reopenSourceStore(t, s)
	if cached() {
		t.Fatal("day cached under the old rule survived")
	}
	save()
	s = reopenSourceStore(t, s)
	if !cached() {
		t.Fatal("recomputed day dropped again")
	}
}

func unknownFor(flag HistoryMissing, ms int64) func(int64, *HistorySources) {
	return func(off int64, v *HistorySources) {
		if off > 0 && off <= ms {
			v.Missing = flag
		}
	}
}

func reopenSourceStore(t *testing.T, s *Store) *Store {
	t.Helper()
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	return s
}
