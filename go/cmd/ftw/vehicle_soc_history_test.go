package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Follow the actual telemetry tick into SQLite, then reopen it. A delayed
// car report keeps its source clock; repeated or cached reports add no level.
func TestVehicleSensorSoCPersistsWithSourceTimeAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("car-sensor")
	observed := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	soc := 0.57
	emit := func(fresh bool) {
		payload, err := json.Marshal(map[string]any{"soc_fresh": fresh, "soc_observed_at_ms": observed.UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		tel.Update("car-sensor", telemetry.DerVehicle, 0, &soc, payload)
	}
	emit(true)
	emit(true) // same source observation, new poll
	soc = 0.99
	emit(false) // cached data is not a new sensor observation
	if _, err := persistTelemetryTick(st, tel, tickPersistControl{}, time.Now().UnixMilli(), time.Minute, testEnergyIdentity(st)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	points, err := st.LoadSeriesBucketsOrRawContext(ctx, "car-sensor", "vehicle_soc", observed.Add(-time.Second).UnixMilli(), time.Now().UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].V != 0.57 || points[0].N != 1 || points[0].FirstMS != observed.UnixMilli() {
		t.Fatalf("sensor level or source time changed across reopen: %+v", points)
	}
}
