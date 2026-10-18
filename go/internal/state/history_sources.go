package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"
)

// HistorySources carries independently fresh site-side powers, never the live
// Load result. Topology identifies the electrical flows included in the sum.
type HistorySources struct {
	AtMS                            int64
	Topology                        string
	Missing                         HistoryMissing
	GridW, PVW, BatteryW, EVW, V2XW float64
}

const sourcePeriodsSchema = `CREATE TABLE IF NOT EXISTS history_source_periods(start_ms INTEGER PRIMARY KEY,last_ms INTEGER NOT NULL) WITHOUT ROWID`

const sourceCursorSchema = `CREATE TABLE IF NOT EXISTS history_source_cursor(key TEXT PRIMARY KEY,value TEXT NOT NULL) WITHOUT ROWID`

// This uses the site energy columns so tariff and day readers share exactly
// the same evidence. load_wh stays zero: Load is derived after interval sums.
// ev_wh is the signed EV+V2X energy, including discharge.
const sourceEnergyTable = "history_source_energy"
const maxSourceEnergyWidth int64 = 900000

func (s *Store) recordSourceEnergyTx(ctx context.Context, tx *sql.Tx, p *HistoryPoint) error {
	if p == nil || p.Sources == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM history_source_cursor WHERE key='last'`)
		return err
	}
	at := p.Sources.AtMS
	if at <= 0 {
		return errors.New("history source snapshot has no observation time")
	}
	var last struct {
		At      int64
		Start   int64
		Sources HistorySources
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM history_source_cursor WHERE key='last'`).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &last); err != nil {
			return err
		}
	}
	var through int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM history_source_cursor WHERE key='through'),0)`).Scan(&through); err != nil {
		return err
	}
	if at <= through {
		if at < through {
			_, err = tx.ExecContext(ctx, `DELETE FROM history_source_cursor WHERE key='last'`)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_source_cursor VALUES('from',?)`, at); err != nil {
		return err
	}
	if last.At == 0 {
		last.Start = at
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_source_periods VALUES(?,?) ON CONFLICT(start_ms) DO UPDATE SET last_ms=excluded.last_ms`, last.Start, at); err != nil {
		return err
	}
	// Hold the previous receipt forward, bounded by the next safe tick. Do
	// not assign a newly arrived charger value to time before its receipt.
	if last.At > 0 && at-last.At <= maxLoadEstimateTickMS && last.Sources.Topology == p.Sources.Topology {
		// A delayed writer may finish a just-closed local day. Invalidate only
		// the nearby cached dates; source rows and old history stay intact.
		low := time.UnixMilli(last.At).UTC().AddDate(0, 0, -1).Format("2006-01-02")
		high := time.UnixMilli(at).UTC().AddDate(0, 0, 1).Format("2006-01-02")
		for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day>=? AND day<=?`, low, high); err != nil {
				return err
			}
		}
		v := last.Sources
		missing := (v.Missing | p.Sources.Missing) & MissingPower
		if missing != 0 {
			missing |= MissingLoad
		}
		for from := last.At; from < at; {
			start := bucketStart(from, ArchiveResolutionMS)
			to := min(at, start+ArchiveResolutionMS)
			ms := to - from
			h := float64(ms) / 3600000
			if _, err := tx.ExecContext(ctx, `INSERT INTO history_source_energy VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(start_ms,resolution_ms) DO UPDATE SET first_ms=MIN(first_ms,excluded.first_ms),last_ms=MAX(last_ms,excluded.last_ms),covered_ms=covered_ms+excluded.covered_ms,n=n+excluded.n,
 import_wh=import_wh+excluded.import_wh,export_wh=export_wh+excluded.export_wh,pv_wh=pv_wh+excluded.pv_wh,bat_charge_wh=bat_charge_wh+excluded.bat_charge_wh,bat_discharge_wh=bat_discharge_wh+excluded.bat_discharge_wh,ev_wh=ev_wh+excluded.ev_wh,ev_charge_wh=ev_charge_wh+excluded.ev_charge_wh,ev_discharge_wh=ev_discharge_wh+excluded.ev_discharge_wh`,
				start, ArchiveResolutionMS, from, to, ms, 1,
				math.Max(0, v.GridW)*h, math.Max(0, -v.GridW)*h, -v.PVW*h,
				math.Max(0, v.BatteryW)*h, math.Max(0, -v.BatteryW)*h, 0, (v.EVW+v.V2XW)*h, (math.Max(0, v.EVW)+math.Max(0, v.V2XW))*h, (math.Max(0, -v.EVW)+math.Max(0, -v.V2XW))*h); err != nil {
				return err
			}
			if err := putHistoryQuality(ctx, tx, "source_energy", start, ArchiveResolutionMS, missing); err != nil {
				return err
			}
			from = to
		}
	}
	last.At, last.Sources = at, *p.Sources
	b, err := json.Marshal(last)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_source_cursor VALUES('last',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO history_source_cursor VALUES('through',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, at)
	return err
}

func (s *Store) sourceEnergyFrom(ctx context.Context) (int64, bool, error) {
	var from int64
	err := s.history.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM history_source_cursor WHERE key='from'`).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return from, err == nil, err
}

// HistorySourceProgress is the last committed receipt time. Once it reaches
// a day boundary, later source integration cannot change that closed day.
func (s *Store) HistorySourceProgress(ctx context.Context) (int64, bool, error) {
	var through int64
	err := s.history.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM history_source_cursor WHERE key='through'`).Scan(&through)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return through, err == nil, err
}

func (s *Store) sourcePeriodCoverage(ctx context.Context, from, to int64) (int64, error) {
	var ms int64
	err := s.history.QueryRowContext(ctx, `SELECT COALESCE(SUM(MIN(last_ms,?)-MAX(start_ms,?)),0) FROM history_source_periods WHERE last_ms>? AND start_ms<?`, to, from, from, to).Scan(&ms)
	return ms, err
}
func (s *Store) sourcePeriods(ctx context.Context, from, to int64) ([][2]int64, error) {
	rows, err := s.history.QueryContext(ctx, `SELECT start_ms,last_ms FROM history_source_periods WHERE start_ms<=? AND last_ms>=? ORDER BY start_ms`, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var v [2]int64
		if err := rows.Scan(&v[0], &v[1]); err != nil {
			return nil, err
		}
		out = append(out, v)
		if len(out) > maxRawSeriesPoints {
			return nil, ErrHistoryQueryLimit
		}
	}
	return out, rows.Err()
}

type sourceLoadBucket struct {
	start, width, first, last, covered, n int64
	wh                                    float64
	missing                               HistoryMissing
}

// Whole source minutes only: never prorate a stored bucket across a gap or a
// price boundary. Signed minutes survive until this requested interval sums.
func (s *Store) sourceLoadBuckets(ctx context.Context, from, to, width int64) ([]sourceLoadBucket, error) {
	rows, err := s.history.QueryContext(ctx, `SELECT e.start_ms,e.resolution_ms,e.first_ms,e.last_ms,e.covered_ms,e.n,
 e.import_wh-e.export_wh+e.pv_wh-e.bat_charge_wh+e.bat_discharge_wh-e.ev_wh,COALESCE(q.missing,0)
 FROM history_source_energy e LEFT JOIN history_quality q ON q.kind='source_energy' AND q.start_ms=e.start_ms AND q.resolution_ms=e.resolution_ms
 WHERE e.start_ms>=? AND e.start_ms<? AND e.start_ms+e.resolution_ms>? ORDER BY e.start_ms`, from-maxSourceEnergyWidth, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byStart := map[int64]*sourceLoadBucket{}
	for rows.Next() {
		var r sourceLoadBucket
		if err := rows.Scan(&r.start, &r.width, &r.first, &r.last, &r.covered, &r.n, &r.wh, &r.missing); err != nil {
			return nil, err
		}
		start := from + (r.start-from)/width*width
		if r.start < from {
			start = from
		}
		w := max(width, r.width)
		b := byStart[start]
		if b == nil {
			b = &sourceLoadBucket{start: start, width: w, first: r.first, last: r.last}
			byStart[start] = b
		}
		if r.first < from || r.last > to || r.first < start || r.last > start+w {
			b.missing |= MissingLoad
			continue
		}
		b.first = min(b.first, r.first)
		b.last = max(b.last, r.last)
		b.covered += r.covered
		b.n += r.n
		b.wh += r.wh
		b.missing |= r.missing
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]sourceLoadBucket, 0, len(byStart))
	for _, b := range byStart {
		// Only a complete common interval is a chart mean. A partial tail
		// stays live-only, never diluted by padding unobserved time with zero.
		if b.covered != b.width || b.first != b.start || b.last != b.start+b.width || b.wh < 0 || !finiteHistoryValue(b.wh) {
			b.missing |= MissingLoad
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out, nil
}

func finiteHistoryValue(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (s *Store) applySourceLoadHistory(ctx context.Context, points []HistoryPoint, from, to, width int64) error {
	cut, ok, err := s.sourceEnergyFrom(ctx)
	if err != nil || !ok || to <= cut {
		return err
	}
	buckets, err := s.sourceLoadBuckets(ctx, from, to, width)
	if err != nil {
		return err
	}
	periods, err := s.sourcePeriods(ctx, from, to)
	if err != nil {
		return err
	}
	byStart := map[int64]sourceLoadBucket{}
	for _, b := range buckets {
		byStart[b.start] = b
	}
	for i := range points {
		p := &points[i]
		if p.TsMs < cut {
			continue
		}
		inPeriod := false
		for _, v := range periods {
			if v[0] <= p.TsMs && p.TsMs <= v[1] {
				inPeriod = true
				break
			}
		}
		if !inPeriod {
			continue
		}
		start := from + (p.TsMs-from)/width*width
		p.Missing &^= MissingLoad | BalanceMismatch | EstimatedLoad
		p.LoadW = 0
		b, exists := byStart[start]
		if !exists || b.covered == 0 || b.missing&MissingPower != 0 {
			p.Missing |= MissingLoad
		} else {
			p.LoadW = b.wh * 3600000 / float64(b.width)
			p.Missing |= EstimatedLoad
		}
		// JSON must not retain the old instantaneous Load alongside the column.
		var detail map[string]any
		if json.Unmarshal([]byte(p.JSON), &detail) == nil && detail != nil {
			detail["load_house_w"] = p.Missing.Value(MissingLoad, p.LoadW)
			detail["history_load_method"] = "source_interval_balance_v1"
			if raw, err := json.Marshal(detail); err == nil {
				p.JSON = string(raw)
			}
		}
	}
	return nil
}

func (s *Store) applySourceLoadLedger(ctx context.Context, q EnergyHistoryQuery, points []EnergyLedgerPoint) ([]EnergyLedgerPoint, bool, error) {
	if q.AssetID != "" {
		return points, false, nil
	}
	cut, ok, err := s.sourceEnergyFrom(ctx)
	if err != nil || !ok || q.UntilMS <= cut {
		return points, false, err
	}
	readTo := q.UntilMS
	if q.BucketMS < (q.UntilMS-q.SinceMS)/int64(q.Limit+1) {
		readTo = q.SinceMS + int64(q.Limit+1)*q.BucketMS
	}
	buckets, err := s.sourceLoadBuckets(ctx, q.SinceMS, readTo, q.BucketMS)
	if err != nil {
		return nil, false, err
	}
	periods, err := s.sourcePeriods(ctx, q.SinceMS, q.UntilMS)
	if err != nil {
		return nil, false, err
	}
	out := points[:0]
	for _, p := range points {
		overlaps := false
		if p.Flow == FlowConsumerUse {
			for _, v := range periods {
				if v[0] < p.BucketStartMS+p.BucketLenMS && v[1] > p.BucketStartMS {
					overlaps = true
					break
				}
			}
		}
		if overlaps {
			continue
		}
		out = append(out, p)
	}
	truncated := false
	for _, b := range buckets {
		if b.covered == 0 || b.missing&MissingPower != 0 {
			continue
		}
		if (b.start-q.SinceMS)/q.BucketMS >= int64(q.Limit) {
			truncated = true
			continue
		}
		out = append(out, EnergyLedgerPoint{SchemaVersion: EnergyLedgerSchemaVersion, AssetID: "system", Flow: FlowConsumerUse, BucketStartMS: b.start, BucketLenMS: b.width, EnergyWh: b.wh, Source: "power_telemetry", Quality: "integrated", Provenance: "source_interval_balance_v1", SampleCount: b.n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BucketStartMS < out[j].BucketStartMS })
	return out, truncated, nil
}
