package state

import (
	"context"
	"testing"
	"time"
)

const quarterMS = int64(15 * time.Minute / time.Millisecond)

func householdQuarters(t *testing.T, s *Store, from, to int64) map[int64]float64 {
	t.Helper()
	got, closed, err := s.HouseholdIntervals(context.Background(), from, to, quarterMS)
	if err != nil {
		t.Fatal(err)
	}
	if closed < from || closed > to || closed%quarterMS != 0 {
		t.Fatalf("closed range ends at %d, outside [%d,%d]", closed, from, to)
	}
	out := map[int64]float64{}
	for _, v := range got {
		if v.EndMS-v.StartMS != quarterMS {
			t.Fatalf("interval width %d", v.EndMS-v.StartMS)
		}
		out[v.StartMS] = v.LoadW
	}
	return out
}

func TestHouseholdIntervalsMatchDailyTotal(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 6*quarterMS
	end := base + 3*quarterMS
	for at := base; at <= end; at += 5000 {
		// House 1200 W during the first quarter, then 800 W; a battery and
		// a charger take part so the balance uses every source.
		grid := 1700.0
		if at >= base+quarterMS {
			grid = 1300
		}
		sourceTick(t, s, at, HistorySources{GridW: grid, BatteryW: 300, EVW: 200})
	}
	got := householdQuarters(t, s, base, end)
	if len(got) != 3 {
		t.Fatalf("quarters=%v", got)
	}
	near(t, "first quarter W", got[base], 1200)
	near(t, "second quarter W", got[base+quarterMS], 800)
	d, err := s.DailyEnergy(base, end)
	if err != nil || d.Missing&MissingLoad != 0 {
		t.Fatal("daily total unavailable", err, d.Missing)
	}
	var wh float64
	for _, w := range got {
		wh += w / 4
	}
	near(t, "daily Wh equals the sum of training intervals", d.LoadWh, wh)
}

func TestHouseholdIntervalsLeaveOutIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		tick func(at, base int64) (HistorySources, bool)
	}{
		// Longer than any gap history may bridge as an estimate.
		{"missing EV", func(at, base int64) (HistorySources, bool) {
			v := HistorySources{GridW: 1000}
			if at >= base+quarterMS+60000 && at < base+quarterMS+240000 {
				v.Missing = MissingEV
			}
			return v, true
		}},
		{"three-minute recording gap", func(at, base int64) (HistorySources, bool) {
			return HistorySources{GridW: 1000}, at <= base+quarterMS+60000 || at >= base+quarterMS+240000
		}},
		{"negative balance", func(at, base int64) (HistorySources, bool) {
			v := HistorySources{GridW: 1000}
			if at >= base+quarterMS && at < base+2*quarterMS {
				v.GridW = -500
			}
			return v, true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sourceStore(t)
			base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 6*quarterMS
			end := base + 3*quarterMS
			for at := base; at <= end; at += 5000 {
				if v, ok := tc.tick(at, base); ok {
					sourceTick(t, s, at, v)
				}
			}
			got := householdQuarters(t, s, base, end)
			if _, ok := got[base+quarterMS]; ok || len(got) != 2 {
				t.Fatalf("incomplete quarter became a training label: %v", got)
			}
			near(t, "neighbour quarter W", got[base], 1000)
			near(t, "neighbour quarter W", got[base+2*quarterMS], 1000)
		})
	}
}

func TestHouseholdIntervalsWaitForCommittedProgress(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 6*quarterMS
	for at := base; at < base+quarterMS; at += 5000 {
		sourceTick(t, s, at, HistorySources{GridW: 1000})
	}
	if got := householdQuarters(t, s, base, base+4*quarterMS); len(got) != 0 {
		t.Fatalf("open quarter returned before its end was committed: %v", got)
	}
	sourceTick(t, s, base+quarterMS, HistorySources{GridW: 1000})
	if got := householdQuarters(t, s, base, base+4*quarterMS); len(got) != 1 {
		t.Fatalf("closed quarter missing: %v", got)
	}
	// A later gap closes the next quarter without a label; callers move past it.
	sourceTick(t, s, base+3*quarterMS, HistorySources{GridW: 1000})
	got, closed, err := s.HouseholdIntervals(context.Background(), base+quarterMS, base+4*quarterMS, quarterMS)
	if err != nil || len(got) != 0 || closed != base+3*quarterMS {
		t.Fatalf("gap quarters: got=%v closed=%d err=%v", got, closed-base, err)
	}
	if _, _, err := s.HouseholdIntervals(context.Background(), base+1, base+quarterMS, quarterMS); err == nil {
		t.Fatal("unaligned start accepted")
	}
}

// History may show Home use across an estimated minute; training may not.
func TestHouseholdIntervalsLeaveOutEstimatedMinutes(t *testing.T) {
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24*time.Hour).UnixMilli() - 6*quarterMS
	end := base + 3*quarterMS
	for at := base; at <= end; at += 5000 {
		sourceTick(t, s, at, HistorySources{GridW: 1000})
	}
	// A bridged or otherwise estimated minute, as a display rule records it.
	if _, err := s.history.Exec(`INSERT INTO history_quality VALUES('source_energy',?,?,?) ON CONFLICT(kind,start_ms,resolution_ms) DO UPDATE SET missing=missing|excluded.missing`,
		base+quarterMS+5*60000, ArchiveResolutionMS, EstimatedLoad); err != nil {
		t.Fatal(err)
	}
	got := householdQuarters(t, s, base, end)
	if _, ok := got[base+quarterMS]; ok || len(got) != 2 {
		t.Fatalf("estimated minute became a training label: %v", got)
	}
	d, err := s.DailyEnergy(base, end)
	if err != nil || d.Missing&MissingLoad != 0 {
		t.Fatal("History lost Home use over an estimated minute", err, d.Missing)
	}
	near(t, "History Wh", d.LoadWh, 3*250)
}
