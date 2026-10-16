package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The former wall/monotonic-clock bug could emit several old quarters from one
// reading. A complete_balance_v1 collector with a two-minute maximum gap cannot
// emit adjacent quarters at the same late capture time. Disk retries cannot
// create this signature: AvailableAtMS freezes before the observation is queued.
// Power is deliberately absent from this test; genuine household consumption
// may exceed the grid fuse when solar or the battery supplies the difference.
func clockJumpObservationCohorts(rows []forecasting.Observation, cutoff, now int64) map[string]int64 {
	type capture struct {
		cohort string
		start  int64
		at     int64
	}
	seen := make(map[capture]bool)
	bad := make(map[string]int64)
	const quarterMS = int64(15 * time.Minute / time.Millisecond)
	const maxGapMS = int64(2 * time.Minute / time.Millisecond)
	for _, o := range rows {
		if !o.LoadKnown || o.Quality != telemetry.ForecastIntervalQuality || o.StartMS < cutoff ||
			o.AvailableAtMS > now || o.EndMS-o.StartMS != quarterMS || o.StartMS%quarterMS != 0 ||
			o.AvailableAtMS-o.EndMS <= maxGapMS || o.Validate() != nil {
			continue
		}
		previous := capture{o.ConfigVersion, o.StartMS - quarterMS, o.AvailableAtMS}
		next := capture{o.ConfigVersion, o.EndMS, o.AvailableAtMS}
		if seen[previous] || seen[next] {
			bad[o.ConfigVersion] = max(bad[o.ConfigVersion], o.AvailableAtMS)
		}
		seen[capture{o.ConfigVersion, o.StartMS, o.AvailableAtMS}] = true
	}
	return bad
}

// Caller holds configMu and learningMu, before either pipeline can be captured.
// Check once per physical identity in this process. The existing durable load
// cutoff makes the recovery idempotent across crashes and future restarts.
func (f *forecastTracker) recoverClockJumpLearningLocked(ctx context.Context) error {
	site := f.learningSiteLocked(f.site())
	identity := site.SiteID + "/" + rustConfigRevision(site)
	if site.IdentityPending || f.store == nil || f.load == nil {
		return nil
	}
	now := f.now().UnixMilli()
	if now <= 0 {
		return errors.New("clock unavailable for forecast learning recovery")
	}
	if f.learningRecoveryChecked == identity && (f.learningRecoveryAfterMS == 0 || now < f.learningRecoveryAfterMS) {
		return nil
	}
	// Finish a saved reset before considering a new one. Never move its cutoff
	// forward while a worker or disk failure still leaves that intent pending.
	if site.LoadLearningStartedMS > 0 && f.learningErrors["load"] != nil {
		return nil
	}
	workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// An RPi may boot with a wall clock older than its archive. Load future
	// rows too, without treating them as available evidence. Cache the next
	// capture time so clock sync can reopen this check without repeated reads.
	rows, err := f.store.LoadForecastObservations(workCtx, 0, math.MaxInt64)
	if err != nil {
		return fmt.Errorf("read clock-jump evidence: %w", err)
	}
	var after int64
	for _, o := range rows {
		if o.AvailableAtMS > now && (after == 0 || o.AvailableAtMS < after) {
			after = o.AvailableAtMS
		}
	}
	bad := clockJumpObservationCohorts(rows, site.LoadLearningStartedMS, now)
	matched := bad[site.Revision] > 0
	if !matched && len(bad) > 0 {
		// Evaluation cohorts change with the worker build or planning policy,
		// while learning survives. An archived site binding proves that an old
		// cohort belongs to this same physical model; other sites stay untouched.
		for cohort := range bad {
			// The identity binding is timeless metadata, not a forecast input.
			// It may have been archived after this pre-sync boot clock.
			binding, err := f.store.LoadForecastCohortSite(workCtx, cohort, math.MaxInt64)
			if err != nil {
				return fmt.Errorf("read clock-jump site binding: %w", err)
			}
			if binding != nil && binding.SiteID == site.SiteID && binding.LearningRevision == rustConfigRevision(site) {
				matched = true
				break
			}
		}
	}
	if !matched {
		f.learningRecoveryChecked = identity
		f.learningRecoveryAfterMS = after
		return nil
	}
	if err = f.restartLearningLocked(workCtx, site, "load", now); err != nil {
		return err
	}
	f.learningRecoveryChecked = identity
	f.learningRecoveryAfterMS = after
	slog.Warn("forecast load learning restarted after invalid clock-jump intervals", "learning_started_ms", now)
	return nil
}
