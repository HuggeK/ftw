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
// EstimatedLoad in Missing marks a snapshot that holds an inferred value, such
// as PV read as 0 W in twilight; every interval it bounds keeps the marker.
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

// maxSourceBridgeMS bounds how long the site meter's last value, or a
// snapshot across a writer pause or process restart, may stand in for
// missing evidence. It shares the history estimate budget of a recovered
// balance episode. A longer gap stays uncovered.
const maxSourceBridgeMS = maxLoadEstimateEpisodeMS

// sourceReceipt is one snapshot. Stopped marks the first one after a
// process stop.
type sourceReceipt struct {
	HistorySources
	Stopped bool `json:",omitempty"`
}

type sourceCursor struct {
	At      int64
	Start   int64
	Sources HistorySources
	// Held is each source's last known value. Its Missing names the sources
	// not yet known since the cursor began. GridAt is when the site meter
	// was last known.
	Held   *HistorySources `json:",omitempty"`
	GridAt int64           `json:",omitempty"`
}

// sourceFlows pairs each source's flag with its value.
func sourceFlows(v *HistorySources) map[HistoryMissing]*float64 {
	return map[HistoryMissing]*float64{MissingGrid: &v.GridW, MissingPV: &v.PVW, MissingBattery: &v.BatteryW, MissingEV: &v.EVW, MissingV2X: &v.V2XW}
}

func (s *Store) recordSourceEnergyTx(ctx context.Context, tx *sql.Tx, p *HistoryPoint) error {
	var last sourceCursor
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
	if p == nil || p.Sources == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM history_source_cursor WHERE key IN ('last','stopped')`)
		return err
	}
	at := p.Sources.AtMS
	if at <= 0 {
		return errors.New("history source snapshot has no observation time")
	}
	var through int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM history_source_cursor WHERE key='through'),0)`).Scan(&through); err != nil {
		return err
	}
	if at <= through {
		if at == through {
			return nil
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM history_source_cursor WHERE key IN ('last','stopped')`)
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_source_cursor VALUES('from',?)`, at); err != nil {
		return err
	}
	if p.Sources.Missing&MissingPower == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO energy_assets(asset_id,device_id,kind,label,read_only,first_seen_ms,last_seen_ms) VALUES(?,'',?,'Observed load',1,?,?)
 ON CONFLICT(asset_id) DO UPDATE SET last_seen_ms=MAX(last_seen_ms,excluded.last_seen_ms)`, ObservedConsumerAssetID, AssetObservedConsumer, at, at); err != nil {
			return err
		}
	}
	cur := sourceReceipt{HistorySources: *p.Sources}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history_source_cursor WHERE key='stopped')`).Scan(&cur.Stopped); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_source_cursor WHERE key='stopped'`); err != nil {
		return err
	}
	switch {
	case last.At == 0:
		last.Start = at
	case at-last.At > maxSourceBridgeMS:
		// The gap stays uncovered. A long stop starts a new period, as an
		// older writer may have run.
		if cur.Stopped {
			last.Start = at
		}
	default:
		rejected, err := holdSourceIntervalTx(ctx, tx, last, cur)
		if err != nil {
			return err
		}
		// A rejected value stays unknown until its source reports again.
		if last.Held != nil {
			last.Held.Missing |= rejected
		}
	}
	if last.Held == nil {
		last.Held = &HistorySources{Missing: MissingPower &^ MissingLoad}
	}
	known := sourceFlows(last.Held)
	for flag, w := range sourceFlows(&cur.HistorySources) {
		if cur.Missing&flag == 0 {
			*known[flag] = *w
			last.Held.Missing &^= flag
		}
	}
	if cur.Missing&MissingGrid == 0 {
		last.GridAt = at
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_source_periods VALUES(?,?) ON CONFLICT(start_ms) DO UPDATE SET last_ms=excluded.last_ms`, last.Start, at); err != nil {
		return err
	}
	last.At, last.Sources = at, cur.HistorySources
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

// holdSourceIntervalTx holds the previous snapshot forward to this one. Do
// not assign a newly arrived charger value to time before its receipt. A
// source unknown in the snapshot keeps its last known value, the site meter
// for at most the bridge budget. Only two fully measured snapshots a safe
// tick apart, of one topology and one process, make a measured interval;
// every other interval carries EstimatedLoad, which readers never clear.
// It returns the held sources the balance rejected.
func holdSourceIntervalTx(ctx context.Context, tx *sql.Tx, last sourceCursor, cur sourceReceipt) (HistoryMissing, error) {
	v := last.Sources
	var missing, held, rejected HistoryMissing
	flows := sourceFlows(&v)
	for flag, w := range flows {
		switch {
		case v.Missing&flag == 0:
		case last.Held == nil || last.Held.Missing&flag != 0 || flag == MissingGrid && cur.AtMS-last.GridAt > maxSourceBridgeMS:
			missing, *w = missing|flag, 0
		default:
			*w = *sourceFlows(last.Held)[flag]
			held |= flag
		}
	}
	// A household never feeds the site. If a held value says it does, the
	// source changed while unknown, such as a charger that finished.
	if held != 0 && v.GridW-v.PVW-v.BatteryW-v.EVW-v.V2XW < 0 {
		rejected = held
		for flag, w := range flows {
			if held&flag != 0 {
				missing, *w = missing|flag, 0
			}
		}
	}
	// A snapshot with an invalid configuration has no household balance.
	if missing != 0 || v.Missing&MissingLoad != 0 {
		missing |= MissingLoad
	}
	if (v.Missing|cur.Missing)&(MissingPower|EstimatedLoad) != 0 || cur.Stopped || cur.AtMS-v.AtMS > maxLoadEstimateTickMS || v.Topology != cur.Topology {
		missing |= EstimatedLoad
	}
	return rejected, writeSourceIntervalTx(ctx, tx, v.AtMS, cur.AtMS, v, missing)
}

func writeSourceIntervalTx(ctx context.Context, tx *sql.Tx, from, until int64, v HistorySources, missing HistoryMissing) error {
	// A delayed writer may finish a just-closed local day. Invalidate only
	// the nearby cached dates; source rows and old history stay intact.
	low := time.UnixMilli(from).UTC().AddDate(0, 0, -1).Format("2006-01-02")
	high := time.UnixMilli(until).UTC().AddDate(0, 0, 1).Format("2006-01-02")
	for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day>=? AND day<=?`, low, high); err != nil {
			return err
		}
	}
	for from < until {
		start := bucketStart(from, ArchiveResolutionMS)
		to := min(until, start+ArchiveResolutionMS)
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
	return nil
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

// sourceDayCoverageMigration recomputes closed source-era days once. They
// were cached under the rule that any unknown interval voids the whole day.
const sourceDayCoverageMigration = "source-day-coverage-v1"

func (s *Store) refreshSourceDayCache(ctx context.Context) error {
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history_migrations WHERE name=?)`, sourceDayCoverageMigration).Scan(&done); err != nil || done {
		return err
	}
	// Day keys are local dates; a day of margin covers any time zone.
	for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day>=(SELECT date(CAST(value AS INTEGER)/1000,'unixepoch','-1 day') FROM history_source_cursor WHERE key='from')`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES(?)`, sourceDayCoverageMigration); err != nil {
		return err
	}
	return tx.Commit()
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
	return s.sourceLoadBucketsLimited(ctx, from, to, width, 0)
}

// A positive limit counts complete, valid buckets, not empty time slots.
// Ordered streaming bounds memory even when an older prefix has no data.
func (s *Store) sourceLoadBucketsLimited(ctx context.Context, from, to, width int64, limit int) ([]sourceLoadBucket, error) {
	rows, err := s.history.QueryContext(ctx, `SELECT e.start_ms,e.resolution_ms,e.first_ms,e.last_ms,e.covered_ms,e.n,
 e.import_wh-e.export_wh+e.pv_wh-e.bat_charge_wh+e.bat_discharge_wh-e.ev_wh,COALESCE(q.missing,0)
 FROM history_source_energy e LEFT JOIN history_quality q ON q.kind='source_energy' AND q.start_ms=e.start_ms AND q.resolution_ms=e.resolution_ms
 WHERE e.start_ms>=? AND e.start_ms<? AND e.start_ms+e.resolution_ms>? ORDER BY e.start_ms`, from-maxSourceEnergyWidth, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceLoadBucket
	var b *sourceLoadBucket
	finish := func() bool {
		if b == nil {
			return false
		}
		// A partial tail stays live-only; never pad missing time with zero.
		if b.covered != b.width || b.first != b.start || b.last != b.start+b.width || b.wh < 0 || !finiteHistoryValue(b.wh) {
			b.missing |= MissingLoad
		}
		if limit == 0 || b.covered > 0 && b.missing&MissingPower == 0 {
			out = append(out, *b)
		}
		return limit > 0 && len(out) >= limit
	}
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
		if b == nil || b.start != start {
			if finish() {
				return out, nil
			}
			b = &sourceLoadBucket{start: start, width: w, first: r.first, last: r.last}
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
	finish()
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
	if q.AssetID != "" && q.AssetID != ObservedConsumerAssetID {
		return points, false, nil
	}
	cut, ok, err := s.sourceEnergyFrom(ctx)
	if err != nil || !ok || q.UntilMS <= cut {
		return points, false, err
	}
	buckets, err := s.sourceLoadBucketsLimited(ctx, q.SinceMS, q.UntilMS, q.BucketMS, q.Limit+1)
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
	for _, b := range buckets {
		asset := "system"
		if q.AssetID != "" {
			asset = q.AssetID
		}
		out = append(out, EnergyLedgerPoint{SchemaVersion: EnergyLedgerSchemaVersion, AssetID: asset, Flow: FlowConsumerUse, BucketStartMS: b.start, BucketLenMS: b.width, EnergyWh: b.wh, Source: "power_telemetry", Quality: "integrated", Provenance: "source_interval_balance_v1", SampleCount: b.n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BucketStartMS < out[j].BucketStartMS })
	rank := 0
	for i, p := range out {
		if i == 0 || p.BucketStartMS != out[i-1].BucketStartMS {
			rank++
		}
		if rank > q.Limit {
			return out[:i], true, nil
		}
	}
	return out, false, nil
}
