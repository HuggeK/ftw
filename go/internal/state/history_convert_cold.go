package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SQLite settings remain authoritative. A cold override lets a stopped copy
// name an external archive or a different mount without rewriting its config.
func betaConversionColdDir(ctx context.Context, cfg *sql.DB, statePath string, override []string) (string, error) {
	if len(override) > 0 && override[0] != "" {
		return availableConversionColdDir(override[0])
	}
	var raw string
	err := cfg.QueryRowContext(ctx, `SELECT value FROM config WHERE key=?`, configurationKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return filepath.Abs("cold")
	}
	if err != nil {
		return "", err
	}
	saved, err := decodeConfiguration(raw)
	if err != nil {
		return "", err
	}
	var doc struct {
		Config *struct {
			State *struct {
				ColdDir string `json:"cold_dir"`
			} `json:"state"`
		} `json:"config"`
	}
	if err := json.Unmarshal(saved.Document, &doc); err != nil {
		return "", err
	}
	if doc.Config == nil {
		return "", errors.New("stored settings have no config")
	}
	if doc.Config.State == nil || doc.Config.State.ColdDir == "" {
		return filepath.Abs("cold")
	}
	// Core resolves explicit relative cold paths from its working directory.
	return availableConversionColdDir(doc.Config.State.ColdDir)
}

func availableConversionColdDir(value string) (string, error) {
	path, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("source cold directory is unavailable; preserve the archives and mount their original path or use -cold for the stopped copy: %s", path)
	}
	return path, nil
}

func conversionColdPaths(cold string) ([]string, error) {
	if cold == "" {
		return nil, nil
	}
	if _, err := os.Stat(cold); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var paths []string
	err := filepath.WalkDir(cold, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cold, path)
		if err != nil {
			return err
		}
		if rel == "diagnostics" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// These files belong to Core's bucket importer, not the long-format
		// sample parser. Keep them for that existing handler and for backup.
		if strings.HasSuffix(rel, ".buckets.parquet") || strings.HasSuffix(rel, ".legacy-buckets.parquet") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("cold archive contains a link; preserve and copy its data into a plain archive directory before conversion")
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".parquet") {
			return nil
		}
		if _, err := time.Parse("2006/01/02.parquet", filepath.ToSlash(rel)); err != nil {
			return fmt.Errorf("unsupported cold archive layout; preserve this file and use its matching format converter: %s", rel)
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func verifyConvertedColdFiles(ctx context.Context, db *sql.DB, cold string) error {
	paths, err := conversionColdPaths(cold)
	if err != nil {
		return err
	}
	for _, path := range paths {
		digest, err := historyFileHashContext(ctx, path)
		if err != nil {
			return err
		}
		var saved string
		if err := db.QueryRowContext(ctx, `SELECT sha256 FROM ts_archive_days WHERE path=?`, parquetSummaryKey(path)).Scan(&saved); err != nil || saved != digest {
			return errors.New("converted history has unverified cold files; preserve this directory and reconvert a stopped pre-conversion backup with its archives")
		}
	}
	return nil
}

// Merge before summaries, by names and timestamp. The selected beta/hot value
// wins on overlap; an archive-only or late row supplements it. A file receipt
// becomes visible only with the fully verified, published history database.
func mergeBetaColdFiles(ctx context.Context, dst *sql.DB, cold string, report func(string)) error {
	paths, err := conversionColdPaths(cold)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	// Remember source coverage before inserting any archive-only row. This
	// survives resume and does not depend on the order of rows within a file.
	if err := snapshotConversionColdHours(ctx, dst); err != nil {
		return err
	}
	for _, path := range paths {
		key := parquetSummaryKey(path)
		w := newConversionWriter(ctx, dst, "cold:"+key)
		if w.complete() {
			w.close()
			continue
		}
		digest, err := historyFileHashContext(ctx, path)
		if err != nil {
			w.close()
			return err
		}
		var copied int64
		err = walkParquetRows(ctx, path, func(batch []parquetSampleRow) error {
			for _, r := range batch {
				if err := validateHistorySamples([]Sample{{Driver: r.Driver, Metric: r.Metric, TsMs: r.TsMs, Value: r.Value}}); err != nil {
					return err
				}
				err := w.write(func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ts_drivers(name) VALUES(?)`, r.Driver); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ts_metrics(name,unit) VALUES(?,NULL)`, r.Metric); err != nil {
						return err
					}
					var d, m int64
					if err := tx.QueryRowContext(ctx, `SELECT d.id,m.id FROM ts_drivers d,ts_metrics m WHERE d.name=? AND m.name=?`, r.Driver, r.Metric).Scan(&d, &m); err != nil {
						return err
					}
					var got float64
					err := tx.QueryRowContext(ctx, `SELECT value FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=?`, d, m, r.TsMs).Scan(&got)
					if err == nil {
						return nil
					}
					if !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					// A source-free summary has no individual timestamps to join.
					// A later observation is disjoint; an older archive-only row
					// could already be in that summary, so do not guess its count.
					var n, last, raw int64
					hour := seriesHourOf(r.TsMs)
					err = tx.QueryRowContext(ctx, `SELECT n,last_ts_ms,raw_n FROM conversion_cold_hours WHERE driver_id=? AND metric_id=? AND hour_ms=?`, d, m, hour).Scan(&n, &last, &raw)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					if err == nil && n > raw && r.TsMs <= last {
						return errors.New("cold row overlaps an older summary without raw timestamps; preserve the sources and use a backup with the original detail")
					}
					if err := (&Store{}).insertSamplesAndHours(ctx, tx, []resolvedSample{{dID: d, mID: m, ts: r.TsMs, v: r.Value}}); err != nil {
						return err
					}
					if err := tx.QueryRowContext(ctx, `SELECT value FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms=?`, d, m, r.TsMs).Scan(&got); err != nil {
						return err
					}
					if got != r.Value {
						return errors.New("cold sample readback differs")
					}
					return nil
				}, len(r.Driver)+len(r.Metric)+32)
				if err != nil {
					return err
				}
				copied++
				if w.rows == 0 && report != nil {
					report(fmt.Sprintf("cold samples: %d", copied))
				}
			}
			return nil
		})
		if err == nil {
			err = w.flush()
		}
		if err == nil {
			_, err = dst.ExecContext(ctx, `INSERT INTO ts_archive_days(path,sha256) VALUES(?,?) ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256`, key, digest)
		}
		if err == nil {
			err = w.finish()
		}
		w.close()
		if err != nil {
			return fmt.Errorf("merge cold sample day: %w", err)
		}
	}
	return nil
}

func snapshotConversionColdHours(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var complete int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversion_progress WHERE name='cold:hour-baseline:complete'`).Scan(&complete); err != nil {
		return err
	}
	if complete != 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS conversion_cold_hours(driver_id INTEGER NOT NULL,metric_id INTEGER NOT NULL,hour_ms INTEGER NOT NULL,n INTEGER NOT NULL,last_ts_ms INTEGER NOT NULL,raw_n INTEGER NOT NULL,PRIMARY KEY(driver_id,metric_id,hour_ms)) WITHOUT ROWID;
 INSERT INTO conversion_cold_hours SELECT driver_id,metric_id,hour_ms,n,last_ts_ms,
 (SELECT COUNT(*) FROM ts_samples r WHERE r.driver_id=h.driver_id AND r.metric_id=h.metric_id AND r.ts_ms>=h.hour_ms AND r.ts_ms<h.hour_ms+3600000) FROM ts_series_hour h;
 INSERT INTO conversion_progress VALUES('cold:hour-baseline:complete','true')`); err != nil {
		return err
	}
	return tx.Commit()
}
