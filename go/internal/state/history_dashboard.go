package state

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// Chart summaries and energy evidence have separate tables. No energy reader
// integrates averaged chart power. The live ledger still uses its original
// device observations and stable asset identities.
const dashboardSchema = `CREATE TABLE IF NOT EXISTS history_dashboard (
 start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,first_ms INTEGER NOT NULL,last_ms INTEGER NOT NULL,n INTEGER NOT NULL,
 grid_w REAL NOT NULL,pv_w REAL NOT NULL,bat_w REAL NOT NULL,load_w REAL NOT NULL,bat_soc REAL NOT NULL,json TEXT NOT NULL,seen_ms BLOB NOT NULL DEFAULT X'',
 PRIMARY KEY(start_ms,resolution_ms)) WITHOUT ROWID`
const siteEnergySchema = `CREATE TABLE IF NOT EXISTS history_site_energy (
 start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,first_ms INTEGER NOT NULL,last_ms INTEGER NOT NULL,covered_ms INTEGER NOT NULL,n INTEGER NOT NULL,
 import_wh REAL NOT NULL,export_wh REAL NOT NULL,pv_wh REAL NOT NULL,bat_charge_wh REAL NOT NULL,bat_discharge_wh REAL NOT NULL,load_wh REAL NOT NULL,ev_wh REAL NOT NULL,
 PRIMARY KEY(start_ms,resolution_ms)) WITHOUT ROWID`
const siteEnergyCursorSchema = `CREATE TABLE IF NOT EXISTS history_site_cursor (key TEXT PRIMARY KEY,value INTEGER NOT NULL) WITHOUT ROWID`

// A missing usable site snapshot breaks integration even if it recovers before
// the maximum gap. The cursor and chart/energy rows share the tick receipt.
func (s *Store) recordDashboardTx(ctx context.Context, tx *sql.Tx, p *HistoryPoint) error {
	if err := s.recordSourceEnergyTx(ctx, tx, p); err != nil {
		return err
	}
	// Older schema-7 Cores read only the original tables. Keep partial rows
	// separate, and clear their cursor so rollback cannot bridge a gap.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_partial_cursor SELECT key,value FROM history_site_cursor`); err != nil {
		return err
	}
	var legacyLast, partialLast int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM history_site_cursor WHERE key='last'),0),COALESCE((SELECT value FROM history_partial_cursor WHERE key='last'),0)`).Scan(&legacyLast, &partialLast); err != nil {
		return err
	}
	if legacyLast > partialLast {
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('last',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, legacyLast); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('missing',0) ON CONFLICT(key) DO UPDATE SET value=0`); err != nil {
			return err
		}
	}
	if p == nil {
		if err := clearLoadEstimatePendingTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_site_cursor VALUES('last',0) ON CONFLICT(key) DO UPDATE SET value=0`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('last',0) ON CONFLICT(key) DO UPDATE SET value=0`)
		return err
	}
	var previous int64
	err := tx.QueryRowContext(ctx, `SELECT value FROM history_partial_cursor WHERE key='last'`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hadCursor := err == nil
	var from int64
	err = tx.QueryRowContext(ctx, `SELECT value FROM history_partial_cursor WHERE key='from'`).Scan(&from)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		// The last raw point belongs to the old accounting path. It can also
		// anchor the first new interval without counting that interval twice.
		if !hadCursor {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts_ms),0) FROM history_hot WHERE ts_ms<?`, p.TsMs).Scan(&previous); err != nil {
				return err
			}
		}
		from = p.TsMs
		if p.Sources == nil && previous > 0 && p.TsMs-previous <= maxCostIntegrationGap.Milliseconds() {
			from = previous
		} else {
			previous = 0
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('from',?)`, from); err != nil {
			return err
		}
	}
	if p.TsMs > previous {
		if previous == 0 || p.TsMs-previous > maxLoadEstimateTickMS {
			if err := clearLoadEstimatePendingTx(ctx, tx); err != nil {
				return err
			}
		}
		if previous > 0 && p.TsMs-previous <= maxCostIntegrationGap.Milliseconds() {
			var previousMissing HistoryMissing
			err := tx.QueryRowContext(ctx, `SELECT value FROM history_partial_cursor WHERE key='missing'`).Scan(&previousMissing)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var previousEligible int
			err = tx.QueryRowContext(ctx, `SELECT value FROM history_partial_cursor WHERE key='residual_eligible'`).Scan(&previousEligible)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := addSiteIntervalEstimateTx(ctx, tx, previous, p, previousMissing, previousEligible == 1); err != nil {
				return err
			}
			if err := addSiteIntervalTx(ctx, tx, previous, p, previousMissing); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('last',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, p.TsMs); err != nil {
			return err
		}
	}
	if p.TsMs > previous {
		eligible := 0
		if _, ok := residualHouseholdW(p); ok {
			eligible = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('residual_eligible',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, eligible); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_partial_cursor VALUES('missing',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, p.Missing); err != nil {
			return err
		}
	}
	if p.TsMs > previous {
		legacyLast := p.TsMs
		if p.Missing != 0 {
			legacyLast = 0
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_site_cursor VALUES('last',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, legacyLast); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_site_cursor SELECT key,value FROM history_partial_cursor WHERE key='from'`); err != nil {
			return err
		}
	}
	if p.Sources != nil {
		// Older writers must not integrate their first receipt backwards over
		// a source-accounted period after rollback. Keep its saved prefix intact.
		if _, err := tx.ExecContext(ctx, `UPDATE history_partial_cursor SET value=0 WHERE key IN ('last','residual_eligible')`); err != nil {
			return err
		}
	}
	table, kind := "history_dashboard", "dashboard"
	if p.Missing != 0 {
		table, kind = "history_dashboard_partial", "partial_dashboard"
	}
	start := bucketStart(p.TsMs, HistoryResolutionMS)
	var n int64
	var seen []byte
	err = tx.QueryRowContext(ctx, `SELECT n,seen_ms FROM `+table+` WHERE start_ms=? AND resolution_ms=?`, start, HistoryResolutionMS).Scan(&n, &seen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	seen, added := addBucketTimestamp(seen, p.TsMs-start)
	if !added {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(start_ms,resolution_ms) DO UPDATE SET first_ms=MIN(first_ms,excluded.first_ms),
 json=CASE WHEN excluded.last_ms>=last_ms THEN excluded.json ELSE json END,last_ms=MAX(last_ms,excluded.last_ms),
 grid_w=(grid_w*n+excluded.grid_w)/ (n+1),pv_w=(pv_w*n+excluded.pv_w)/(n+1),bat_w=(bat_w*n+excluded.bat_w)/(n+1),load_w=(load_w*n+excluded.load_w)/(n+1),bat_soc=(bat_soc*n+excluded.bat_soc)/(n+1),n=n+1,seen_ms=excluded.seen_ms`,
		start, HistoryResolutionMS, p.TsMs, p.TsMs, 1, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON, seen)
	if err != nil {
		return err
	}
	return putHistoryQuality(ctx, tx, kind, start, HistoryResolutionMS, p.Missing)
}

func addSiteIntervalTx(ctx context.Context, tx *sql.Tx, previous int64, p *HistoryPoint, previousMissing HistoryMissing) error {
	point := *p
	point.Missing |= previousMissing
	point.ClearMissingValues()
	p = &point
	table, kind := "history_site_energy", "energy"
	// SoC does not enter the power balance. Keep its quality bit, but do
	// not mix otherwise known power into a failed-balance energy bucket.
	if p.Missing&^MissingSoC != 0 {
		table, kind = "history_site_energy_partial", "partial_energy"
	}
	for from := previous; from < p.TsMs; {
		start := bucketStart(from, ArchiveResolutionMS)
		to := min(p.TsMs, start+ArchiveResolutionMS)
		ms := to - from
		var intervals int64
		// A split interval contributes one observation, at its end. Counting
		// every minute fragment would make sparse data look well sampled.
		if to == p.TsMs {
			intervals = 1
		}
		hours := float64(ms) / 3600000
		_, err := tx.ExecContext(ctx, `INSERT INTO `+table+` VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(start_ms,resolution_ms) DO UPDATE SET first_ms=MIN(first_ms,excluded.first_ms),last_ms=MAX(last_ms,excluded.last_ms),covered_ms=covered_ms+excluded.covered_ms,n=n+excluded.n,
 import_wh=import_wh+excluded.import_wh,export_wh=export_wh+excluded.export_wh,pv_wh=pv_wh+excluded.pv_wh,bat_charge_wh=bat_charge_wh+excluded.bat_charge_wh,bat_discharge_wh=bat_discharge_wh+excluded.bat_discharge_wh,load_wh=load_wh+excluded.load_wh,ev_wh=ev_wh+excluded.ev_wh`,
			start, ArchiveResolutionMS, from, to, ms, intervals, math.Max(0, p.GridW)*hours, math.Max(0, -p.GridW)*hours, -p.PVW*hours, math.Max(0, p.BatW)*hours, math.Max(0, -p.BatW)*hours, p.LoadW*hours, siteEVWh(p, hours))
		if err != nil {
			return err
		}
		if err := putHistoryQuality(ctx, tx, kind, start, ArchiveResolutionMS, p.Missing); err != nil {
			return err
		}
		from = to
	}
	return nil
}

func (s *Store) siteEnergyFrom(ctx context.Context) (int64, bool, error) {
	var from int64
	err := s.history.QueryRowContext(ctx, `SELECT value FROM history_partial_cursor WHERE key='from' UNION ALL SELECT value FROM history_site_cursor WHERE key='from' LIMIT 1`).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return from, err == nil, err
}

func siteEVWh(p *HistoryPoint, hours float64) float64 {
	if p.Missing&(MissingGrid|MissingPV|MissingBattery|MissingLoad|MissingEV|MissingV2X) != 0 {
		return 0
	}
	return math.Max(0, p.GridW-p.BatW-p.PVW-p.LoadW) * hours
}

type siteEnergyRow struct {
	evCharge, evDischarge                     float64
	source                                    bool
	estimateLoad                              float64
	estimateCovered                           int64
	missing                                   HistoryMissing
	start, width, first, last, covered, n     int64
	imp, exp, pv, charge, discharge, load, ev float64
}

// Include only evidence whose observed bounds fit the request. An edge which
// cuts a stored minute stays uncovered; it is never prorated across gaps or
// treated as constant power. Daily and tariff boundaries use whole minutes.
func (s *Store) walkSiteEnergy(ctx context.Context, from, to int64, visit func(siteEnergyRow) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := s.history.QueryContext(ctx, `WITH energy AS (
 SELECT *, 0 AS ev_charge_wh,0 AS ev_discharge_wh,'energy' AS kind FROM history_site_energy e WHERE NOT EXISTS(SELECT 1 FROM history_source_periods sp WHERE sp.start_ms<e.last_ms AND sp.last_ms>e.first_ms)
 UNION ALL SELECT *,0 AS ev_charge_wh,0 AS ev_discharge_wh,'partial_energy' AS kind FROM history_site_energy_partial e WHERE NOT EXISTS(SELECT 1 FROM history_source_periods sp WHERE sp.start_ms<e.last_ms AND sp.last_ms>e.first_ms)
 UNION ALL SELECT *, 'source_energy' AS kind FROM history_source_energy)
 SELECT e.start_ms,e.resolution_ms,e.first_ms,e.last_ms,e.covered_ms,e.n,e.import_wh,e.export_wh,e.pv_wh,e.bat_charge_wh,e.bat_discharge_wh,e.load_wh,e.ev_wh,COALESCE(q.missing,0),
 CASE WHEN e.kind='partial_energy' AND (q.missing & ~16) IN (136,40) THEN COALESCE((SELECT SUM(load_wh) FROM history_load_estimates x WHERE x.start_ms>=e.start_ms AND x.start_ms+x.resolution_ms<=e.start_ms+e.resolution_ms),0) ELSE 0 END,
 CASE WHEN e.kind='partial_energy' AND (q.missing & ~16) IN (136,40) THEN COALESCE((SELECT SUM(covered_ms) FROM history_load_estimates x WHERE x.start_ms>=e.start_ms AND x.start_ms+x.resolution_ms<=e.start_ms+e.resolution_ms),0) ELSE 0 END,e.kind,e.ev_charge_wh,e.ev_discharge_wh
 FROM energy e LEFT JOIN history_quality q ON q.kind=e.kind AND q.start_ms=e.start_ms AND q.resolution_ms=e.resolution_ms
 WHERE e.start_ms>=? AND e.start_ms<? AND e.first_ms>=? AND e.last_ms<=? ORDER BY e.first_ms`, bucketStart(from, EnergyLedgerDailyBucketMS), to, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r siteEnergyRow
		var kind string
		if err := rows.Scan(&r.start, &r.width, &r.first, &r.last, &r.covered, &r.n, &r.imp, &r.exp, &r.pv, &r.charge, &r.discharge, &r.load, &r.ev, &r.missing, &r.estimateLoad, &r.estimateCovered, &kind, &r.evCharge, &r.evDischarge); err != nil {
			return err
		}
		r.source = kind == "source_energy"
		if r.source {
			r.load = r.imp - r.exp + r.pv - r.charge + r.discharge - r.ev
			r.ev = r.evCharge
			if r.missing&MissingPower != 0 {
				r.load = 0
			} else {
				r.missing |= EstimatedLoad
			}
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// maxSourceDayUnknownShare is the share of a source-era range a flow may be
// unknown or unrecorded before its total is unknown. Below it the total is
// an estimate of what was recorded; unrecorded time counts as nothing.
const maxSourceDayUnknownShare = 10 // percent

func (s *Store) aggregateDayEnergy(ctx context.Context, from, to int64) (DayEnergy, error) {
	cut, hasSources, err := s.sourceEnergyFrom(ctx)
	if err != nil {
		return DayEnergy{}, err
	}
	var out DayEnergy
	var originalLoad float64
	var originalMissing HistoryMissing
	var estimatedMS, coveredMS int64
	var sourceMS, sourceEraMS int64
	unknownMS := map[HistoryMissing]int64{}
	err = s.walkSiteEnergy(ctx, from, to, func(r siteEnergyRow) error {
		out.ImportWh += r.imp
		out.ExportWh += r.exp
		out.PVWh += r.pv
		out.BatChargedWh += r.charge
		out.BatDischargedWh += r.discharge
		originalLoad += r.load
		originalMissing |= r.missing
		coveredMS += r.covered
		if hasSources && r.first >= cut {
			sourceEraMS += r.covered
		}
		if r.missing&^MissingSoC == MissingLoad|BalanceMismatch || r.missing&^MissingSoC == MissingLoad|MissingEV {
			if r.estimateCovered > 0 && r.estimateCovered == r.covered {
				r.load, r.missing = r.estimateLoad, (r.missing&MissingSoC)|EstimatedLoad
				estimatedMS += r.covered
			}
		}
		out.LoadWh += r.load
		out.Intervals += r.n
		if r.covered > 0 {
			out.observed |= MissingPower &^ r.missing
		}
		if r.source {
			// Source rows decide each flow by its unknown time below.
			sourceMS += r.covered
			for _, flag := range []HistoryMissing{MissingGrid, MissingPV, MissingBattery, MissingLoad, MissingEV, MissingV2X} {
				if r.missing&flag != 0 {
					unknownMS[flag] += r.covered
				}
			}
			r.missing &^= MissingPower
		}
		out.Missing |= r.missing
		return nil
	})
	if err != nil {
		return out, err
	}
	if sourceMS == 0 && (out.LoadWh < 0 || (estimatedMS > 120000 && estimatedMS > coveredMS/20)) {
		out.LoadWh = originalLoad
		out.Missing = originalMissing
	}
	if out.LoadWh < 0 {
		out.LoadWh = 0
		out.Missing |= MissingLoad
	}
	if hasSources && to > cut {
		// The writer's last receipt ends the range, so a live tail is not a gap.
		through, _, err := s.HistorySourceProgress(ctx)
		if err != nil {
			return out, err
		}
		span := max(0, min(to, through)-max(from, cut))
		// Unrecorded time leaves the other totals as recorded, as before;
		// the household balance needs it covered.
		unknownMS[MissingLoad] += max(0, span-sourceEraMS)
		for flag, ms := range unknownMS {
			if ms*100 > span*maxSourceDayUnknownShare {
				out.Missing |= flag
			}
		}
	}
	return out, nil
}
