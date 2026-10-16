package api

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A driver card shows a car's level and limit in percent, whether the
// driver sends fractions (the Home Assistant driver) or percents (Tesla's).
func TestTheDriverCardShowsAVehicleLevelInPercent(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tel := telemetry.NewStore()
	for name, data := range map[string]string{
		"tesla-ha":    `{"soc":0.14,"charge_limit":0.8,"soc_fresh":true}`,
		"tesla-cloud": `{"soc":14,"charge_limit_pct":80,"soc_fresh":true}`,
	} {
		var v struct {
			SoC float64 `json:"soc"`
		}
		_ = json.Unmarshal([]byte(data), &v)
		soc := v.SoC
		if soc > 1 {
			soc /= 100
		}
		tel.Update(name, telemetry.DerVehicle, 0, &soc, json.RawMessage(data))
		tel.DriverHealthMut(name).RecordSuccess()
	}
	srv := New(&Deps{State: st, Tel: tel, Ctrl: &control.State{}, CtrlMu: &sync.Mutex{}, CapMu: &sync.RWMutex{}, CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
	rr := httptest.NewRecorder()
	srv.handleStatus(rr, httptest.NewRequest("GET", "/api/status", nil))
	var resp struct {
		Drivers map[string]map[string]any `json:"drivers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tesla-ha", "tesla-cloud"} {
		d := resp.Drivers[name]
		soc, _ := d["vehicle_soc"].(float64)
		limit, _ := d["vehicle_charge_limit_pct"].(float64)
		if math.Abs(soc-14) > 1e-9 || math.Abs(limit-80) > 1e-9 {
			t.Errorf("%s card: soc %v limit %v, want 14 and 80", name, d["vehicle_soc"], d["vehicle_charge_limit_pct"])
		}
	}
}
