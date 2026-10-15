package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

const legacyChartRecovery = "legacy-charts-recovered-v1"

// Old raw retirement omitted inherited charts. The bound state database keeps
// its original source tables even after cold-file cleanup removes raw-retired.
// Recover only charts; keep new measurements and accounting in place.
func (s *Store) recoverLegacyCharts(ctx context.Context, generation string) error {
	var complete int
	if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_migrations WHERE name=?`, legacyChartRecovery).Scan(&complete); err != nil || complete != 0 {
		return err
	}
	rows, err := s.mergeLegacyChartSource(ctx, s.db, filepath.Base(s.mainDBPath))
	if err != nil {
		return fmt.Errorf("recover inherited charts from bound state: %w", err)
	}
	if rows != 0 {
		slog.Info("inherited charts recovered", "source", filepath.Base(s.mainDBPath), "verified_rows", rows)
	}
	paths, err := filepath.Glob(s.historyPath + ".raw-retired*")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
			continue
		}
		rows, same, err := s.recoverRetiredCharts(ctx, path, generation)
		if err != nil {
			return fmt.Errorf("recover inherited charts from %s: %w", filepath.Base(path), err)
		}
		if same {
			slog.Info("inherited charts recovered", "source", filepath.Base(path), "verified_rows", rows)
		}
	}
	_, err = s.history.ExecContext(ctx, `INSERT OR IGNORE INTO history_migrations(name) VALUES(?)`, legacyChartRecovery)
	return err
}

func (s *Store) recoverRetiredCharts(ctx context.Context, path, generation string) (int64, bool, error) {
	source, err := sql.Open("sqlite", ReadOnlyDatabaseURI(path))
	if err != nil {
		return 0, false, err
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	var marker string
	err = source.QueryRowContext(ctx, `SELECT name FROM history_migrations WHERE name LIKE 'generation:%'`).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && marker != "generation:"+generation) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	count, err := s.mergeLegacyChartSource(ctx, source, filepath.Base(path))
	return count, true, err
}

func (s *Store) mergeLegacyChartSource(ctx context.Context, source *sql.DB, name string) (int64, error) {
	var count int64
	for _, table := range []string{"history_hot", "history_warm", "history_cold"} {
		var exists int
		if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return count, err
		}
		if exists == 0 {
			continue
		}
		if s.historyMigration != nil {
			s.historyMigration.update(func(st *HistoryMigrationStatus) {
				st.State, st.Phase, st.Activity = "running", "sqlite", "importing"
				st.CurrentSource = name + "/" + table
				st.CurrentSourceRowsDone = 0
				if st.StartedAtMS == 0 {
					st.StartedAtMS = time.Now().UnixMilli()
				}
			})
		}
		n, err := s.mergeRetiredChartTable(ctx, source, table)
		if err != nil {
			return count, err
		}
		count += n
	}
	return count, nil
}

// Each bounded transaction reads back its rows before commit. A retry can
// replay verified chunks; a conflicting current point stops recovery.
func (s *Store) mergeRetiredChartTable(ctx context.Context, source *sql.DB, table string) (int64, error) {
	var tx *sql.Tx
	var pending, pendingBytes int
	var verified int64
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	count, err := scanHistoryTable(ctx, source, table, func(values []any) error {
		if tx == nil {
			var err error
			tx, err = s.history.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+table+` VALUES(?,?,?,?,?,?,?)`, values...); err != nil {
			return err
		}
		actual := make([]any, len(values))
		pointers := make([]any, len(values))
		for i := range actual {
			pointers[i] = &actual[i]
		}
		if err := tx.QueryRowContext(ctx, `SELECT ts_ms,grid_w,pv_w,bat_w,load_w,bat_soc,json FROM `+table+` WHERE ts_ms=?`, values[0]).Scan(pointers...); err != nil {
			return err
		}
		before, after := sha256.New(), sha256.New()
		if err := hashHistoryRow(before, values); err != nil {
			return err
		}
		if err := hashHistoryRow(after, actual); err != nil {
			return err
		}
		if !bytes.Equal(before.Sum(nil), after.Sum(nil)) {
			return fmt.Errorf("current %s point %v differs from retained source", table, values[0])
		}
		pending++
		verified++
		pendingBytes += conversionRowBytes(values)
		if pending >= 1024 || pendingBytes >= 1<<20 {
			if err := tx.Commit(); err != nil {
				return err
			}
			tx, pending, pendingBytes = nil, 0, 0
			if s.historyMigration != nil {
				s.historyMigration.update(func(st *HistoryMigrationStatus) { st.CurrentSourceRowsDone = verified })
			}
		}
		return nil
	})
	if err == nil && tx != nil {
		err = tx.Commit()
		tx = nil
	}
	return count, err
}
