package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestSourceHistoryDayCachesWaitForDurableBoundary(t *testing.T) {
	for _, firstBatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing method", true: "first source batch"}[firstBatch], func(t *testing.T) { testSourceHistoryDayCaches(t, firstBatch) })
	}
}

func testSourceHistoryDayCaches(t *testing.T, firstBatch bool) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: st, Cfg: &config.Config{Price: &config.Price{Zone: "SE3"}}, CfgMu: &sync.RWMutex{}})
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixMilli()
	base := midnight - 60000
	tick := func(at int64) {
		t.Helper()
		p := state.HistoryPoint{TsMs: at, Missing: state.MissingLoad, JSON: `{}`, Sources: &state.HistorySources{AtMS: at, Topology: "grid", GridW: 1000}}
		if err := st.RecordTickWithEnergy(p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) float64 {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatal("history API failed", rr.Code)
		}
		var result struct {
			Days []struct {
				Load *float64 `json:"load_wh"`
			} `json:"days"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Days) != 2 {
			t.Fatal("missing day windows")
		}
		if result.Days[0].Load == nil {
			return 0
		}
		return *result.Days[0].Load
	}
	if firstBatch {
		for _, path := range []string{"/api/energy/daily?days=2", "/api/savings/daily?days=2"} {
			_ = read(path)
		}
	}
	for i := int64(0); i < 6; i++ {
		tick(base + i*10000)
	}
	if !firstBatch {
		for _, path := range []string{"/api/energy/daily?days=2", "/api/savings/daily?days=2"} {
			_ = read(path)
		}
		day := time.UnixMilli(base).In(now.Location()).Format("2006-01-02")
		if _, ok := srv.dailyCache[day]; ok {
			t.Fatal("energy cached an uncommitted closed day")
		}
		if _, ok := srv.savingsCache[day]; ok {
			t.Fatal("savings cached an uncommitted closed day")
		}
	}
	tick(midnight)
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if firstBatch {
		// An older request finishes saving after the first source commit.
		// Its stale total must not refill the persistent cache under a new RAM key.
		day := time.UnixMilli(base).In(now.Location()).Format("2006-01-02")
		if err := st.SaveDailyEnergyForSourceMode(day, state.DayEnergy{LoadWh: 1, Intervals: 1}, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/energy/daily?days=2", "/api/savings/daily?days=2"} {
		if got := read(path); math.Abs(got-1000.0/60) > 1e-8 {
			t.Fatal("same API server retained Load from before final commit")
		}
	}
}
