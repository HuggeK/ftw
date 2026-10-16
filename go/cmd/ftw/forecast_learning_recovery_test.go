package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/modelstate"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Synthetic complete intervals with one impossible shared capture time. No
// customer identifiers, measurements, model state or timestamps belong here.
func clockJumpObservations(site forecastSite, start time.Time) []forecasting.Observation {
	rows := make([]forecasting.Observation, 2)
	for i := range rows {
		rows[i] = learningObservation(site, start.Add(time.Duration(i)*15*time.Minute))
		rows[i].Quality = telemetry.ForecastIntervalQuality
		rows[i].AvailableAtMS = start.Add(time.Hour).UnixMilli()
		rows[i].LoadW = float64(20+i*15) * 1000
	}
	return rows
}

func TestForecastClockJumpRecoverySignature(t *testing.T) {
	start := time.Now().Add(-4 * time.Hour).Truncate(15 * time.Minute)
	base := clockJumpObservations(hostForecastSite(), start)
	now := start.Add(3 * time.Hour).UnixMilli()
	tests := []struct {
		name   string
		change func([]forecasting.Observation)
		cutoff int64
		want   bool
	}{
		{"shared capture", nil, 0, true},
		{"low power still has impossible time", func(o []forecasting.Observation) { o[0].LoadW, o[1].LoadW = 700, 800 }, 0, true},
		{"ordinary high household power", func(o []forecasting.Observation) {
			for i := range o {
				o[i].AvailableAtMS = o[i].EndMS + 1000
				o[i].LoadW = 107000
			}
		}, 0, false},
		{"exactly maximum valid delay", func(o []forecasting.Observation) {
			for i := range o {
				o[i].AvailableAtMS = o[i].EndMS + 120000
			}
		}, 0, false},
		{"only one late quarter", func(o []forecasting.Observation) { o[0].AvailableAtMS = o[0].EndMS }, 0, false},
		{"different captures", func(o []forecasting.Observation) { o[0].AvailableAtMS++ }, 0, false},
		{"different cohorts", func(o []forecasting.Observation) { o[0].ConfigVersion = "other" }, 0, false},
		{"missing quarter", func(o []forecasting.Observation) { o[0].StartMS -= 900000; o[0].EndMS -= 900000 }, 0, false},
		{"same quarter twice", func(o []forecasting.Observation) { o[0] = o[1] }, 0, false},
		{"PV only", func(o []forecasting.Observation) { o[0].LoadKnown = false }, 0, false},
		{"other quality", func(o []forecasting.Observation) { o[0].Quality = "import" }, 0, false},
		{"partial interval", func(o []forecasting.Observation) { o[0].StartMS++ }, 0, false},
		{"NaN", func(o []forecasting.Observation) { o[0].LoadW = math.NaN() }, 0, false},
		{"infinite", func(o []forecasting.Observation) { o[0].LoadW = math.Inf(1) }, 0, false},
		{"future capture", func(o []forecasting.Observation) {
			for i := range o {
				o[i].AvailableAtMS = now + 1
			}
		}, 0, false},
		{"already reset", nil, start.Add(2 * time.Hour).UnixMilli(), false},
		{"cutoff straddles evidence", nil, start.Add(time.Minute).UnixMilli(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := append([]forecasting.Observation(nil), base...)
			if tc.change != nil {
				tc.change(rows)
			}
			got := clockJumpObservationCohorts(rows, tc.cutoff, now)
			if (len(got) > 0) != tc.want {
				t.Fatalf("cohorts=%v want corruption=%v", got, tc.want)
			}
			rows[0], rows[1] = rows[1], rows[0]
			if !reflect.DeepEqual(got, clockJumpObservationCohorts(rows, tc.cutoff, now)) {
				t.Fatal("row order changed diagnosis")
			}
		})
	}
}

func saveClockJumpEvidence(t *testing.T, st *state.Store, rows []forecasting.Observation) {
	t.Helper()
	for _, row := range rows {
		if err := st.SaveForecastObservation(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
}

func seedLegacyRecoveryModel(t *testing.T, st *state.Store, site forecastSite, at time.Time) {
	t.Helper()
	for _, profile := range loadmodel.Profiles() {
		m := loadmodel.NewModel(4000)
		m.ConfigRevision, m.Timezone = rustConfigRevision(site), site.Timezone
		m.Update(at, 35000, 5)
		data, err := modelstate.Wrap(loadmodel.FeatureHash(), m)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveConfig("loadmodel/state_utc:"+string(profile), data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForecastClockJumpRecoveryKeepsHealthyLearning(t *testing.T) {
	for _, kind := range []string{"healthy", "different learning identity", "different site", "unbound older cohort"} {
		t.Run(kind, func(t *testing.T) {
			st := hostForecastDB(t)
			site := hostForecastSite()
			start := time.Now().Add(-4 * time.Hour).Truncate(time.Hour)
			seedLegacyRecoveryModel(t, st, site, start)
			rows := clockJumpObservations(site, start)
			if kind == "healthy" {
				for i := range rows {
					rows[i].AvailableAtMS = rows[i].EndMS + 1000
					rows[i].LoadW = 70000
				}
			} else {
				for i := range rows {
					rows[i].ConfigVersion = "old-worker-cohort"
				}
				if kind != "unbound older cohort" {
					issue := hostForecastIssue(start.Add(2*time.Hour), start.Add(90*time.Minute), 1)
					issue.ConfigVersion = rows[0].ConfigVersion
					issue.Site = &forecasting.SiteContext{SiteID: site.SiteID, LearningRevision: rustConfigRevision(site), Timezone: site.Timezone}
					if kind == "different site" {
						issue.Site.SiteID = "different-site"
					} else {
						issue.Site.LearningRevision = "different-learning"
					}
					if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
						t.Fatal(err)
					}
				}
			}
			saveClockJumpEvidence(t, st, rows)
			f := learningTracker(st, &site, start.Add(3*time.Hour), nil)
			before := f.load.Snapshot()
			if err := f.restoreLearning(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, f.load.Snapshot()) {
				t.Fatal("unproven corruption changed healthy model")
			}
			if _, ok := st.LoadConfig(forecastLearningKey); ok {
				t.Fatal("unproven corruption persisted a reset")
			}
		})
	}
}

func TestForecastClockJumpRecoveryFromBackupRetainsEvidenceAndGoals(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Now().Add(-4 * time.Hour).Truncate(time.Hour)
	rows := clockJumpObservations(site, start)
	seedLegacyRecoveryModel(t, st, site, start)
	saveClockJumpEvidence(t, st, rows)
	goal := `{"soc":0.8,"time_of_day_min_utc":300,"recurring":true}`
	if err := st.SaveConfig("loadpoint_schedule:garage", goal); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.gz")
	if err := st.BackupToCompressed(backup); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	z, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	data, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	now := start.Add(3 * time.Hour)
	f := learningTracker(restored, &site, now, nil)
	beforePV := f.pv.Model()
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, profile := range f.load.Snapshot().Profiles {
		if profile.Samples != 0 || profile.LearningStartedMS != now.UnixMilli() {
			t.Fatalf("poisoned restored profile survived: %+v", profile)
		}
	}
	if !reflect.DeepEqual(beforePV, f.pv.Model()) {
		t.Fatal("load recovery changed PV")
	}
	got, err := restored.LoadForecastObservations(context.Background(), start.UnixMilli(), now.UnixMilli())
	if err != nil || !reflect.DeepEqual(got, rows) {
		t.Fatalf("raw archive changed: %+v %v", got, err)
	}
	if got, _ := restored.LoadConfig("loadpoint_schedule:garage"); got != goal {
		t.Fatal("EV goal changed")
	}
	// A second process sees the durable cutoff, not a fresh recovery epoch.
	f = learningTracker(restored, &site, now.Add(time.Hour), nil)
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.load.LearningStartedMS() != now.UnixMilli() {
		t.Fatal("restart repeated reset")
	}
}

func TestForecastClockJumpRecoveryWaitsForBootClockSync(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Now().Add(-4 * time.Hour).Truncate(time.Hour)
	saveClockJumpEvidence(t, st, clockJumpObservations(site, start))
	seedLegacyRecoveryModel(t, st, site, start)
	now := start.Add(-time.Hour)
	f := learningTracker(st, &site, now, nil)
	f.clock = func() time.Time { return now }
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.load.LearningStartedMS() != 0 {
		t.Fatal("future archive reset load before clock sync")
	}
	if f.learningRecoveryAfterMS != start.Add(time.Hour).UnixMilli() {
		t.Fatal("pre-sync startup permanently skipped future evidence")
	}
	now = start.Add(3 * time.Hour)
	f.reconcileLearning(context.Background())
	if f.load.LearningStartedMS() != now.UnixMilli() {
		t.Fatal("same-identity clock sync did not reopen recovery")
	}
	cutoff := f.load.LearningStartedMS()
	now = now.Add(time.Hour)
	f.reconcileLearning(context.Background())
	if f.load.LearningStartedMS() != cutoff {
		t.Fatal("clock sync caused repeated resets")
	}
}

func TestForecastClockJumpRecoveryWaitsForIdentityAndArchive(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Now().Add(-4 * time.Hour).Truncate(time.Hour)
	saveClockJumpEvidence(t, st, clockJumpObservations(site, start))
	seedLegacyRecoveryModel(t, st, site, start)
	site.IdentityPending = true
	f := learningTracker(st, &site, start.Add(3*time.Hour), nil)
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.load.LearningStartedMS() != 0 {
		t.Fatal("unconfirmed hardware identity reset learning")
	}
	site.IdentityPending = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.reconcileLearning(ctx)
	if f.load.LearningStartedMS() != 0 || f.learningRecoveryChecked != "" {
		t.Fatal("unavailable archive caused a reset or consumed recovery check")
	}
	f.reconcileLearning(context.Background())
	if f.load.LearningStartedMS() != start.Add(3*time.Hour).UnixMilli() {
		t.Fatal("valid identity/archive did not recover")
	}
}

func TestForecastClockJumpRecoveryNativeCrashAndWorkerUpdate(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	site.LearningRevision = "physical-site-stable"
	start := time.Now().Add(-4 * time.Hour).Truncate(time.Hour)
	rows := clockJumpObservations(site, start)
	saveClockJumpEvidence(t, st, rows)
	r := learningNative(t, st)
	for _, row := range rows {
		if err := r.Update(context.Background(), site, row, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	beforePV := learningModel(t, r, "pv")
	beforeLoad := learningModel(t, r, "load")
	// Save the old cohort's physical identity before a new worker changes the
	// evaluation revision. The private model remains opaque to Core.
	issue := hostForecastIssue(start.Add(2*time.Hour), start.Add(90*time.Minute), 1)
	issue.Site = &forecasting.SiteContext{SiteID: site.SiteID, LearningRevision: site.LearningRevision, Timezone: site.Timezone}
	if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	site.Revision = "new-worker-cohort"
	now := start.Add(3 * time.Hour)
	f := learningTracker(st, &site, now, r)
	transport := r.transport.(energyforecast.RoundTripper)
	r.client = energyforecast.NewClient(hostForecastExchange(func(ctx context.Context, p []byte) ([]byte, error) {
		var header struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(p, &header)
		if header.Action == "reset" {
			return nil, errors.New("injected worker outage after durable intent")
		}
		return transport.RoundTrip(ctx, p)
	}))
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.learningPeriods.LoadMS != now.UnixMilli() || f.learningPeriods.PVMS != 0 {
		t.Fatal("recovery intent absent or crossed signals")
	}
	if !bytes.Equal(beforeLoad, learningModel(t, r, "load")) {
		t.Fatal("failed worker reset published changed state")
	}
	// Simulate a crash between durable reset intent and the worker state save.
	r = learningNative(t, st)
	f = learningTracker(st, &site, now.Add(time.Hour), r)
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.learningPeriods.LoadMS != now.UnixMilli() || f.load.LearningStartedMS() != now.UnixMilli() {
		t.Fatal("crash retry moved reset cutoff")
	}
	if !bytes.Equal(beforePV, learningModel(t, r, "pv")) {
		t.Fatal("recovery changed PV state")
	}
	if bytes.Equal(beforeLoad, learningModel(t, r, "load")) {
		t.Fatal("recovery left poisoned load state")
	}
	if got := f.LearningStatus("load"); got.Status != "cold_start" || got.LatestTrainingMS != 0 {
		t.Fatalf("recovery forged training: %+v", got)
	}
	eligible := learningObservation(site, now.Add(15*time.Minute))
	if err := f.updateObservation(context.Background(), forecastObservationJob{site: site, observation: eligible}); err != nil {
		t.Fatal(err)
	}
	learned := r.Snapshot()
	r = learningNative(t, st)
	f = learningTracker(st, &site, now.Add(2*time.Hour), r)
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(learned, r.Snapshot()) {
		t.Fatal("second restart erased healthy post-recovery learning")
	}
	if got := f.LearningStatus("load"); got.LatestTrainingMS != eligible.EndMS {
		t.Fatalf("healthy learning lost: %+v", got)
	}
}
