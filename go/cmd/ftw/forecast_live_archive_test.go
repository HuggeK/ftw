package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The live meter replaces the in-flight slot's load on every replan. That is
// a measurement, not Core's recovery, so the archive keeps the model's source
// for it and for every later slot whose forecast passed unchanged.
func TestForecastArchiveKeepsModelSourceUnderLiveLoad(t *testing.T) {
	at := time.Now().UTC().Truncate(15 * time.Minute)
	f := primaryFixture(at, primaryReply)
	st, err := state.Open(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var prices []state.PricePoint
	for i := 0; i < 8; i++ {
		prices = append(prices, state.PricePoint{Zone: "SE3", SlotTsMs: at.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100 + float64(i)*10, Source: "test", FetchedAtMs: at.UnixMilli()})
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := mpc.New(st, nil, "SE3", mpc.Params{Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: .5, SoCMin: .1, SoCMax: .9, MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: .95, DischargeEfficiency: .95})
	svc.BaseLoad = 500
	svc.ForecastSnapshot = f.Snapshot
	svc.HouseholdMeasurement = func() telemetry.ForecastReading {
		return telemetry.ForecastReading{HouseholdW: 2345, Valid: true}
	}
	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatalf("no plan: %+v", svc.PlanSnapshot())
	}
	issue := (<-f.queue).issue
	points := primarySeries(t, issue, "champion").Points
	if len(points) < 4 {
		t.Fatalf("short archive: %d points", len(points))
	}
	if points[0].LoadW != 2345 {
		t.Fatalf("slot 0 load %g, want the live 2345 W", points[0].LoadW)
	}
	for i, p := range points[:4] {
		if p.LoadSource != "energyplan" || p.ModelLoad == nil || p.LoadQuality == "fallback" {
			t.Fatalf("slot %d attributed to recovery: source=%q quality=%q model=%v", i, p.LoadSource, p.LoadQuality, p.ModelLoad)
		}
	}
	if points[1].LoadW == points[2].LoadW {
		t.Fatalf("fixture loads do not vary: %+v", points[:3])
	}
	if err := issue.Validate(); err != nil {
		t.Fatal(err)
	}
}
