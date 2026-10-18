package state

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func replayLoadPoint(at int64, w float64) *HistoryPoint {
	p := &HistoryPoint{TsMs: at, GridW: 1000, BatW: 1000 - w, LoadW: w, JSON: `{"ev_w":0,"v2x_w":0}`}
	if w < 0 {
		p.LoadW = 0
		p.Missing = MissingLoad | BalanceMismatch
		p.JSON = `{"ev_w":0,"v2x_w":0,"history_reason":"invalid_balance"}`
	}
	return p
}

func TestLoadEstimateWaitsForShortRecovery(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	dashboardTick(t, s, "first", replayLoadPoint(base, 1000))
	dashboardTick(t, s, "negative", replayLoadPoint(base+4000, -3946))
	d, err := s.DailyEnergy(base, base+60000)
	if err != nil || d.Missing&MissingLoad == 0 {
		t.Fatal("unclosed episode supplied a total", d, err)
	}
	dashboardTick(t, s, "recovered", replayLoadPoint(base+8000, 5768))
	d, err = s.DailyEnergy(base, base+60000)
	// Preserve signed balance: (-3946 + 5768) W * 4 s. Averaging the
	// surrounding valid 1000/5768 W endpoints would invent 7.52 Wh.
	want := float64(-3946+5768) * 4 / 3600
	if err != nil || d.Missing != EstimatedLoad || math.Abs(d.LoadWh-want) > 1e-9 {
		t.Fatal(d, err, want)
	}
	// A retried tick must not count the recovered estimate twice.
	dashboardTick(t, s, "recovered", replayLoadPoint(base+8000, 5768))
	d2, err := s.DailyEnergy(base, base+60000)
	if err != nil || d2.LoadWh != d.LoadWh {
		t.Fatal(d2, err)
	}
}

func TestLoadEstimateRejectsLongUncertainAndOfflineEpisodes(t *testing.T) {
	for _, scenario := range []string{"long_episode", "long_tick", "missing_source", "offline_tick", "restart", "out_of_order"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			if err = s.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
			dashboardTick(t, s, "start", replayLoadPoint(base, 1000))
			dashboardTick(t, s, "bad", replayLoadPoint(base+5000, -1000))
			recovery := int64(10000)
			switch scenario {
			case "long_episode":
				for sec := 10; sec <= 130; sec += 5 {
					dashboardTick(t, s, fmt.Sprint(sec), replayLoadPoint(base+int64(sec)*1000, -1000))
				}
				recovery = 135000
			case "long_tick":
				recovery = 25000
			case "missing_source":
				p := replayLoadPoint(base+7000, 1000)
				p.Missing = MissingLoad | MissingEV
				p.JSON = `{"ev_w":null,"v2x_w":0,"history_reason":"offline:ev"}`
				dashboardTick(t, s, "offline", p)
			case "offline_tick":
				dashboardTick(t, s, "nil", nil)
			case "restart":
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			case "out_of_order":
				// An older valid tick cannot close the pending failure.
				dashboardTick(t, s, "old", replayLoadPoint(base+2000, 2000))
				d, err := s.DailyEnergy(base, base+60000)
				if err != nil || d.Missing&MissingLoad == 0 {
					t.Fatal(d, err)
				}
				return
			}
			dashboardTick(t, s, "recovery", replayLoadPoint(base+recovery, 3000))
			d, err := s.DailyEnergy(base, base+60000)
			if err != nil || d.Missing&MissingLoad == 0 {
				t.Fatal("unsafe episode supplied a total", d, err)
			}
		})
	}
}

func TestLoadEstimateSplitsAtLocalMidnightAndRetainsSignedEnergy(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	midnight := time.Date(2026, 10, 18, 0, 0, 0, 0, loc).UnixMilli()
	for i, w := range []float64{1000, -1000, 3000, 1000} {
		p := replayLoadPoint(midnight-15000+int64(i)*10000, w)
		p.Missing |= MissingSoC // An unrelated SoC gap must not block power evidence.
		dashboardTick(t, s, fmt.Sprint(i), p)
	}
	a, err := s.DailyEnergy(midnight-86400000, midnight)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.DailyEnergy(midnight, midnight+60000)
	if err != nil {
		t.Fatal(err)
	}
	// The first half has a positive signed total: -10 kWs + 15 kWs.
	near(t, "before midnight", a.LoadWh, 5000.0/3600)
	near(t, "after midnight", b.LoadWh, 25000.0/3600)
	if a.Missing&EstimatedLoad == 0 || b.Missing&EstimatedLoad == 0 {
		t.Fatal(a, b)
	}
	coverage, err := s.HistoryEnergyCoverage(context.Background(), midnight-60000, midnight+60000)
	if err != nil || coverage != 30000 {
		t.Fatal(coverage, err)
	}
}

func TestHomeEnergyRestartLeavesStoppedTimeUncovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for i := 0; i < 2; i++ {
		dashboardTick(t, s, fmt.Sprint(i), replayLoadPoint(base+int64(i)*5000, 1000))
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 4; i++ {
		dashboardTick(t, s, fmt.Sprint(i), replayLoadPoint(base+int64(i)*5000, 1000))
	}
	coverage, err := s.HistoryEnergyCoverage(context.Background(), base, base+60000)
	if err != nil || coverage != 10000 {
		t.Fatal("restart interval counted as measured", coverage, err)
	}
	d, err := s.DailyEnergy(base, base+60000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "recorded home energy", d.LoadWh, 1000*10.0/3600)
}

func TestMissingSoCDoesNotInflateEstimatedPowerCoverage(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	for sec := 0; sec <= 600; sec += 5 {
		w := 1000.0
		if sec%120 == 65 {
			w = -1000
		}
		if sec%120 == 70 {
			w = 3000
		}
		p := replayLoadPoint(base+int64(sec)*1000, w)
		p.Missing |= MissingSoC
		dashboardTick(t, s, fmt.Sprint(sec), p)
	}
	d, err := s.DailyEnergy(base, base+600000)
	if err != nil || d.Missing != EstimatedLoad|MissingSoC {
		t.Fatal(d, err)
	}
	near(t, "signed energy without SoC", d.LoadWh, 1000*600.0/3600)
}

func TestLoadEstimateCanCloseOneMinuteImbalance(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	var signedWS float64
	for sec := 0; sec <= 375; sec += 5 {
		w := 4000.0
		if sec >= 305 && sec <= 360 {
			w = -4000
		}
		p := replayLoadPoint(base+int64(sec)*1000, w)
		dashboardTick(t, s, fmt.Sprint(sec), p)
		if sec > 0 {
			signedWS += w * 5
		}
	}
	d, err := s.DailyEnergy(base, base+420000)
	if err != nil || d.Missing != EstimatedLoad {
		t.Fatal(d, err)
	}
	near(t, "signed minute-lag energy", d.LoadWh, signedWS/3600)
}
