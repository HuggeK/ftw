package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"
)

// Estimates supplement, never overwrite, the original measurements and quality.
// Signed residuals are integrated before taking a daily total: clipping each
// negative instantaneous balance biases the total upwards. Forecast training
// and control continue to reject those instantaneous samples.
const loadEstimateSchema = `CREATE TABLE IF NOT EXISTS history_load_estimates (
 start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,covered_ms INTEGER NOT NULL,
 load_wh REAL NOT NULL,method TEXT NOT NULL,PRIMARY KEY(start_ms,resolution_ms)) WITHOUT ROWID`

// Pending evidence never enters totals. A valid balance must close a short
// episode before any of its intervals can supplement the original history.
const loadEstimatePendingSchema = `CREATE TABLE IF NOT EXISTS history_load_estimate_pending (
 from_ms INTEGER PRIMARY KEY,to_ms INTEGER NOT NULL,load_w REAL NOT NULL) WITHOUT ROWID`

// LoadEstimateRecoveryWindow bounds a recovered balance episode, including a
// minute-scale cloud lag. It is a history estimate budget, not a sensor delay.
const LoadEstimateRecoveryWindow = 2 * time.Minute
const maxLoadEstimateEpisodeMS = int64(LoadEstimateRecoveryWindow / time.Millisecond)
const maxLoadEstimateTickMS int64 = 15_000

func residualHouseholdW(p *HistoryPoint) (float64, bool) {
	missing := p.Missing &^ MissingSoC
	if missing != 0 && missing != MissingLoad|BalanceMismatch {
		return 0, false
	}
	var evidence struct {
		EV     *float64 `json:"ev_w"`
		V2X    *float64 `json:"v2x_w"`
		Reason string   `json:"history_reason"`
	}
	if json.Unmarshal([]byte(p.JSON), &evidence) != nil || evidence.EV == nil || evidence.V2X == nil {
		return 0, false
	}
	if missing != 0 && evidence.Reason != "invalid_balance" {
		return 0, false
	}
	w := p.GridW - p.PVW - p.BatW - *evidence.EV - *evidence.V2X
	return w, !math.IsNaN(w) && !math.IsInf(w, 0)
}

func addSiteIntervalEstimateTx(ctx context.Context, tx *sql.Tx, previous int64, p *HistoryPoint, previousMissing HistoryMissing, previousEligible bool) error {
	// An offline, duplicate, skewed or otherwise missing source is not repaired
	// by interpreting it as zero. Both endpoints need usable source evidence.
	missing := (p.Missing | previousMissing) &^ MissingSoC
	w, ok := residualHouseholdW(p)
	if missing != MissingLoad|BalanceMismatch || !previousEligible || !ok || p.TsMs-previous > maxLoadEstimateTickMS {
		return clearLoadEstimatePendingTx(ctx, tx)
	}
	var start int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(from_ms),?) FROM history_load_estimate_pending`, previous).Scan(&start); err != nil {
		return err
	}
	if p.TsMs-start > maxLoadEstimateEpisodeMS {
		// Keep a marker until a valid snapshot arrives. Discarding only the
		// rows would let a long failure look like repeated short episodes.
		if err := clearLoadEstimatePendingTx(ctx, tx); err != nil {
			return err
		}
		if p.Missing&MissingLoad == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO history_load_estimate_pending VALUES(?, ?, 0)`, start, p.TsMs)
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_load_estimate_pending VALUES(?,?,?)`, previous, p.TsMs, w); err != nil {
		return err
	}
	if p.Missing&MissingLoad != 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT from_ms,to_ms,load_w FROM history_load_estimate_pending ORDER BY from_ms`)
	if err != nil {
		return err
	}
	type interval struct {
		from, to int64
		watts    float64
	}
	var intervals []interval
	for rows.Next() {
		var v interval
		if err := rows.Scan(&v.from, &v.to, &v.watts); err != nil {
			rows.Close()
			return err
		}
		intervals = append(intervals, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, v := range intervals {
		if err := writeLoadEstimateTx(ctx, tx, v.from, v.to, v.watts); err != nil {
			return err
		}
	}
	return clearLoadEstimatePendingTx(ctx, tx)
}

func clearLoadEstimatePendingTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM history_load_estimate_pending`)
	return err
}

func writeLoadEstimateTx(ctx context.Context, tx *sql.Tx, previous, until int64, w float64) error {
	for from := previous; from < until; {
		start := bucketStart(from, ArchiveResolutionMS)
		to := min(until, start+ArchiveResolutionMS)
		covered := to - from
		_, err := tx.ExecContext(ctx, `INSERT INTO history_load_estimates VALUES(?,?,?,?,?)
   ON CONFLICT(start_ms,resolution_ms) DO UPDATE SET covered_ms=covered_ms+excluded.covered_ms,load_wh=load_wh+excluded.load_wh`, start, ArchiveResolutionMS, covered, w*float64(covered)/3600000, "signed_source_balance_v1")
		if err != nil {
			return err
		}
		from = to
	}
	return nil
}

// HistoryEnergyCoverage reports actual integrated time, excluding writer stalls
// and offline gaps. It does not extrapolate energy into unrecorded periods.
func (s *Store) HistoryEnergyCoverage(ctx context.Context, from, to int64) (int64, error) {
	var covered int64
	err := s.walkSiteEnergy(ctx, from, to, func(r siteEnergyRow) error { covered += r.covered; return nil })
	return covered, err
}
