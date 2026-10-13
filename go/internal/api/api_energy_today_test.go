package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEnergyTodayExposesMeasuredEnergyAfterPVPollGap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pv         float64
		allMissing bool
	}{
		{"producing after recovery", -600, false},
		{"measured zero after recovery", 0, false},
		{"never measured", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			if now.Sub(midnight) < 4*time.Minute {
				t.Skip("need four elapsed minutes in the current day")
			}
			for i := 0; i < 4; i++ {
				p := state.HistoryPoint{TsMs: midnight.Add(time.Duration(i) * time.Minute).UnixMilli(), GridW: 1200, PVW: tc.pv, BatW: 200, LoadW: 1000 - tc.pv, JSON: `{}`}
				if tc.allMissing || i == 1 {
					p.Missing = state.MissingPV | state.MissingLoad
					p.ClearMissingValues()
				}
				if err := st.EnqueueTelemetryTick(&p, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.FlushHistory(context.Background()); err != nil {
				t.Fatal(err)
			}
			srv := New(&Deps{State: st, Tel: telemetry.NewStore(), Ctrl: &control.State{}, CtrlMu: &sync.Mutex{}, CapMu: &sync.RWMutex{}, CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
			rr := httptest.NewRecorder()
			srv.handleStatus(rr, httptest.NewRequest("GET", "/api/status", nil))
			var resp struct {
				Energy struct {
					Today map[string]any `json:"today"`
				} `json:"energy"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			today := resp.Energy.Today
			if today["pv_wh"] != nil || today["load_wh"] != nil {
				t.Fatal("partial energy claimed as a complete day", today)
			}
			if today["history_quality"] != "incomplete" {
				t.Fatal("missing day quality", today)
			}
			measured, ok := today["measured"].(map[string]any)
			if !ok {
				t.Fatal("known energy hidden after a brief PV gap", today)
			}
			if tc.allMissing {
				if measured["pv_wh"] != nil || measured["load_wh"] != nil {
					t.Fatal("unmeasured flow became a measured zero", measured)
				}
			} else {
				for key, want := range map[string]float64{"pv_wh": -tc.pv / 60, "load_wh": (1000 - tc.pv) / 60} {
					got, ok := measured[key].(float64)
					if !ok || math.Abs(got-want) > 1e-9 {
						t.Fatalf("%s measured=%v, want %v without filling the gap", key, measured[key], want)
					}
				}
			}
			if math.Abs(today["import_wh"].(float64)-60) > 1e-9 {
				t.Fatal("independent meter energy changed", today)
			}
		})
	}
}
