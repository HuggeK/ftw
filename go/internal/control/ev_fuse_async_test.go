package control

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type externalAuditRawEvent struct {
	kind       telemetry.DerType
	secondsAgo int
	watts      float64
	confirmed  bool
}

func TestAsynchronousPowerCredit(t *testing.T) {
	cases := []struct {
		name           string
		events         []externalAuditRawEvent
		minCap, maxCap float64
	}{
		{"steady older EV and newer site", []externalAuditRawEvent{{telemetry.DerEV, 8, 10000, false}, {telemetry.DerMeter, 7, 13000, false}, {telemetry.DerEV, 4, 10000, false}, {telemetry.DerMeter, 3, 13000, false}}, 10000, 14250},
		{"steady newer EV and older site", []externalAuditRawEvent{{telemetry.DerEV, 8, 10000, false}, {telemetry.DerMeter, 7, 13000, false}, {telemetry.DerMeter, 5, 13000, false}, {telemetry.DerEV, 4, 10000, false}}, 10000, 14250},
		{"new EV rise cannot be subtracted from older site", []externalAuditRawEvent{{telemetry.DerEV, 8, 0, false}, {telemetry.DerMeter, 7, 1000, false}, {telemetry.DerEV, 4, 11000, false}}, 0, 16250},
		{"old high EV then observed stop", []externalAuditRawEvent{{telemetry.DerEV, 8, 11000, false}, {telemetry.DerMeter, 7, 12000, false}, {telemetry.DerEV, 4, 0, false}, {telemetry.DerMeter, 3, 1000, false}}, 0, 16250},
		{"qualified EV permits trimming a small total overload", []externalAuditRawEvent{{telemetry.DerEV, 8, 10000, false}, {telemetry.DerMeter, 7, 17750, false}, {telemetry.DerEV, 4, 10000, false}}, 1380, 9500},
		{"fresh actual overload beats an older stable bracket", []externalAuditRawEvent{{telemetry.DerEV, 8, 10000, false}, {telemetry.DerMeter, 7, 13000, false}, {telemetry.DerEV, 4, 10000, false}, {telemetry.DerMeter, 3, 30000, false}}, 0, 0},
		{"confirmed unchanged EV supports normal asynchronous site", []externalAuditRawEvent{{telemetry.DerEV, 8, 10000, false}, {telemetry.DerEV, 4, 10000, true}, {telemetry.DerMeter, 3, 13000, false}}, 10000, 14250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			events := append([]externalAuditRawEvent(nil), tc.events...)
			sort.SliceStable(events, func(i, j int) bool { return events[i].secondsAgo > events[j].secondsAgo })
			store := telemetry.NewStore()
			for _, e := range events {
				driver := "meter"
				if e.kind == telemetry.DerEV {
					driver = "car"
				}
				data, _ := json.Marshal(map[string]any{"control_power_observed_at": now.Add(-time.Duration(e.secondsAgo) * time.Second).Format(time.RFC3339Nano), "control_power_available": true, "control_power_confirmed": e.confirmed})
				store.Update(driver, e.kind, e.watts, nil, data)
				store.DriverHealthMut(driver).RecordSuccess()
			}
			st := NewState(0, 0, "meter")
			st.SiteFuseAmps = 25
			st.SiteFuseVoltage = 230
			st.SiteFusePhases = 3
			capW, known := EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p"}}, 17250)
			if !known || capW < tc.minCap-0.001 || capW > tc.maxCap+0.001 {
				t.Fatalf("cap=%v known=%v want[%v,%v]", capW, known, tc.minCap, tc.maxCap)
			}
		})
	}
}

func TestExistingPhaseOverloadTrimsKnownThreePhaseEV(t *testing.T) {
	now := time.Now()
	store := telemetry.NewStore()
	emit := func(driver string, kind telemetry.DerType, at time.Time, w float64, amps bool) {
		data := map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano), "control_power_available": true}
		if amps {
			data["l1_a"] = 26.0
			data["l2_a"] = 5.0
			data["l3_a"] = 5.0
		}
		b, _ := json.Marshal(data)
		store.Update(driver, kind, w, nil, b)
		store.DriverHealthMut(driver).RecordSuccess()
	}
	emit("car", telemetry.DerEV, now.Add(-8*time.Second), 10350, false)
	emit("meter", telemetry.DerMeter, now.Add(-7*time.Second), 8280, true)
	emit("car", telemetry.DerEV, now.Add(-4*time.Second), 10350, false)
	st := NewState(0, 0, "meter")
	st.SiteFuseAmps = 25
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	capW, known := EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p"}}, 17250)
	// Current EV is15A on each phase; its underlying L1 net load is11A.
	// A24A operating ceiling permits at most13A (=8970W) of EV charge.
	if !known || capW <= 0 || capW > 8970.001 {
		t.Fatalf("known3p phase overload needs safe positive trim≤8970W, cap=%v known=%v", capW, known)
	}
}

func TestObservedZeroCannotDisappearFromPowerCredit(t *testing.T) {
	store := telemetry.NewStore()
	at := time.Now().Add(-4 * time.Second).Truncate(time.Second)
	emit := func(driver string, kind telemetry.DerType, stamp time.Time, w float64) {
		d, _ := json.Marshal(map[string]any{"control_power_observed_at": stamp.Format(time.RFC3339Nano), "control_power_available": true})
		store.Update(driver, kind, w, nil, d)
		store.RecordDriverSuccess(driver)
	}
	emit("car", telemetry.DerEV, at.Add(-100*time.Millisecond), 11000)
	emit("car", telemetry.DerEV, at.Add(100*time.Millisecond), 0)
	emit("meter", telemetry.DerMeter, at.Add(500*time.Millisecond), 1000)
	emit("car", telemetry.DerEV, at.Add(900*time.Millisecond), 11000)
	st := NewState(0, 0, "meter")
	st.SiteFuseAmps = 25
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	capW, known := EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p"}}, 17250)
	if !known || capW > 16250.001 {
		t.Fatalf("observed zero erased around site1kW: cap=%v known=%v, must≤16250W", capW, known)
	}
}
