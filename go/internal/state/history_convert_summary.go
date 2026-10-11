package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const convertedChartSummaries = "beta-chart-summaries-v1"
const incompleteConvertedCharts = "converted history lacks chart summaries; preserve this directory and run conversion on a stopped pre-conversion backup"

// The cursor and summaries commit together. Replay cannot add a sample twice,
// and no destination becomes active before the complete dataset is summarized.
func summarizeConvertedSamples(ctx context.Context, db *sql.DB, report func(string)) error {
	const phase = "samples:summary"
	var saved string
	err := db.QueryRowContext(ctx, `SELECT cursor FROM conversion_progress WHERE name=?`, phase).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if saved == "complete" {
		return nil
	}
	var cursor [3]int64
	if saved != "" {
		if err := json.Unmarshal([]byte(saved), &cursor); err != nil {
			return err
		}
	}
	s := &Store{history: db}
	var completed int64
	for {
		query := `SELECT driver_id,metric_id,ts_ms,value FROM ts_samples`
		var args []any
		if saved != "" {
			query += ` WHERE (driver_id,metric_id,ts_ms)>(?,?,?)`
			args = []any{cursor[0], cursor[1], cursor[2]}
		}
		rows, err := db.QueryContext(ctx, query+` ORDER BY driver_id,metric_id,ts_ms LIMIT 2048`, args...)
		if err != nil {
			return err
		}
		batch := make([]resolvedSample, 0, 2048)
		for rows.Next() {
			var r resolvedSample
			if err := rows.Scan(&r.dID, &r.mID, &r.ts, &r.v); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := s.aggregateSamples(ctx, tx, batch, false); err != nil {
			tx.Rollback()
			return err
		}
		last := batch[len(batch)-1]
		cursor = [3]int64{last.dID, last.mID, last.ts}
		encoded, err := json.Marshal(cursor)
		if err == nil {
			saved = string(encoded)
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,?)`, phase, saved)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		completed += int64(len(batch))
		if report != nil {
			report(fmt.Sprintf("summarize samples: %d", completed))
		}
	}
	// Old summaries can contain measurements whose raw rows already expired.
	// Keep those complete hours. Replace only an absent or smaller summary;
	// adding the raw aggregate would count the overlapping samples twice.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ts_series_hour SELECT * FROM ts_aggregate_hours WHERE true
 ON CONFLICT(driver_id,metric_id,hour_ms) DO UPDATE SET
 sum_value=excluded.sum_value,min_value=excluded.min_value,max_value=excluded.max_value,
 n=excluded.n,last_ts_ms=excluded.last_ts_ms WHERE excluded.n>ts_series_hour.n`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_migrations(name) VALUES(?)`, seriesHoursMigration); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_migrations(name) VALUES(?)`, convertedChartSummaries); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO conversion_progress VALUES(?,'complete')`, phase); err != nil {
		return err
	}
	return tx.Commit()
}
