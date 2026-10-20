package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// householdHistoryStore records source ticks as Core does, so load
// observations come from the same closed intervals as daily Home use.
func householdHistoryStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// recordSiteHistory writes 5 s source ticks over [from, to]. The house uses
// gridW - pvW.
func recordSiteHistory(t *testing.T, st *state.Store, from, to time.Time, gridW, pvW float64) {
	t.Helper()
	for at := from; !at.After(to); at = at.Add(5 * time.Second) {
		v := state.HistorySources{AtMS: at.UnixMilli(), Topology: "site", GridW: gridW, PVW: pvW}
		if err := st.RecordTickWithOptionalHistory(&state.HistoryPoint{TsMs: at.UnixMilli(), JSON: `{}`, Sources: &v}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// pvReading carries a deliberately wrong instantaneous house value: load
// labels must come from history, never from this snapshot.
func pvReading(at time.Time) telemetry.ForecastReading {
	return telemetry.ForecastReading{
		At: at, Earliest: at, Latest: at, PVEarliest: at, PVLatest: at,
		Valid: true, PVValid: true, HouseholdW: 9999, PVW: -800,
	}
}

func TestForecastObservationUsesCurrentTelemetryCutoff(t *testing.T) {
	ctx := context.Background()
	st := householdHistoryStore(t)
	tel := telemetry.NewStore()
	site := trackerSite()
	f := trackerFixture(time.Now())
	f.store = st
	f.tele = tel
	f.site = func() forecastSite { return site }
	var observedAt time.Time
	f.curtailed = func(at time.Time) bool {
		observedAt = at
		return false
	}
	feed := func(at time.Time) []forecasting.Observation {
		return f.observationIntervals(ctx, pvReading(at), site, at)
	}

	// Prime the real PV accumulator from the start of the current quarter
	// through the scheduled tick. A synchronous archive retry can delay
	// observe until a newer telemetry sample has arrived.
	scheduledAt := time.Now().UTC()
	quarterStart := scheduledAt.Truncate(15 * time.Minute)
	if out := feed(quarterStart); len(out) != 0 {
		t.Fatalf("first sample completed an interval: %+v", out)
	}
	for at := quarterStart.Add(time.Minute); at.Before(scheduledAt); at = at.Add(time.Minute) {
		if out := feed(at); len(out) != 0 {
			t.Fatalf("partial quarter completed early: %+v", out)
		}
	}
	if scheduledAt.After(quarterStart) {
		if out := feed(scheduledAt); len(out) != 0 {
			t.Fatalf("scheduled sample completed early: %+v", out)
		}
	}

	tel.Update("site", telemetry.DerMeter, 400, nil, nil)
	tel.Update("pv", telemetry.DerPV, -800, nil, nil)
	tel.RecordDriverSuccess("site")
	tel.RecordDriverSuccess("pv")
	f.observe(ctx)
	if observedAt.IsZero() {
		t.Fatal("observation did not apply the curtailment cutoff")
	}

	var completed []forecasting.Observation
	quarterEnd := quarterStart.Add(15 * time.Minute)
	recordSiteHistory(t, st, quarterStart, quarterEnd, 400, -800)
	if observedAt.Before(quarterEnd) {
		for at := observedAt.Add(time.Minute); at.Before(quarterEnd); at = at.Add(time.Minute) {
			completed = append(completed, feed(at)...)
		}
		completed = append(completed, feed(quarterEnd)...)
	}
	persisted, err := st.LoadForecastObservations(ctx, quarterStart.UnixMilli(), quarterEnd.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	completed = append(completed, persisted...)
	if len(completed) != 1 || !completed[0].LoadKnown || completed[0].Quality != state.HouseholdIntervalQuality ||
		!completed[0].PVKnown || math.Abs(completed[0].LoadW-1200) > 1e-6 || math.Abs(completed[0].PVW-800) > 1e-6 ||
		completed[0].StartMS != quarterStart.UnixMilli() || completed[0].EndMS != quarterEnd.UnixMilli() {
		t.Fatalf("delayed observation lost or changed the complete quarter: %+v", completed)
	}
}

func TestForecastPVStillRejectsLongGap(t *testing.T) {
	f := &forecastTracker{}
	site := trackerSite()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if out := f.observationIntervals(ctx, pvReading(start), site, start); len(out) != 0 {
		t.Fatalf("first sample completed an interval: %+v", out)
	}
	if out := f.observationIntervals(ctx, pvReading(start.Add(3*time.Minute)), site, start.Add(3*time.Minute)); len(out) != 0 {
		t.Fatalf("long gap produced an interval: %+v", out)
	}
	var completed []forecasting.Observation
	for at := start.Add(4 * time.Minute); !at.After(start.Add(15 * time.Minute)); at = at.Add(time.Minute) {
		completed = append(completed, f.observationIntervals(ctx, pvReading(at), site, at)...)
	}
	if len(completed) != 0 {
		t.Fatalf("quarter with a long gap was accepted: %+v", completed)
	}
}

// PV waits for history to decide its quarter, so each quarter is archived
// once. If history stays silent, PV goes alone and a late load is dropped.
func TestForecastPVWaitsForTheHouseholdDecision(t *testing.T) {
	ctx := context.Background()
	st := householdHistoryStore(t)
	f := &forecastTracker{store: st}
	site := trackerSite()
	start := time.Now().UTC().Truncate(15 * time.Minute).Add(-2 * time.Hour)
	end := start.Add(15 * time.Minute)
	feed := func(at time.Time) []forecasting.Observation {
		return f.observationIntervals(ctx, pvReading(at), site, at)
	}
	for at := start; at.Before(end); at = at.Add(time.Minute) {
		if out := feed(at); len(out) != 0 {
			t.Fatalf("partial quarter emitted: %+v", out)
		}
	}
	recordSiteHistory(t, st, start, end.Add(-time.Minute), 400, -800)
	if out := feed(end); len(out) != 0 {
		t.Fatalf("PV emitted before history decided its quarter: %+v", out)
	}
	if out := feed(end.Add(time.Minute)); len(out) != 0 {
		t.Fatalf("PV emitted inside the wait: %+v", out)
	}
	out := feed(end.Add(maxHouseholdDecisionWait))
	if len(out) != 1 || out[0].LoadKnown || !out[0].PVKnown || out[0].StartMS != start.UnixMilli() {
		t.Fatalf("PV lost after silent history: %+v", out)
	}
	recordSiteHistory(t, st, end.Add(-time.Minute+5*time.Second), end.Add(time.Minute), 400, -800)
	if out := feed(end.Add(maxHouseholdDecisionWait + time.Minute)); len(out) != 0 {
		t.Fatalf("a decided quarter was emitted twice: %+v", out)
	}
}
