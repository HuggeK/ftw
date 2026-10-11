package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestForecastRetentionLargeOverflowWithinScoringBudget(t *testing.T) {
	s := openForecastArchive(t)
	now := time.Now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO forecast_errors VALUES('load','fixture',1,?,?,?,?,'fixture',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 1024)
	for i := 0; i < MaxForecastErrors+4096; i++ {
		if _, err := stmt.Exec(now-int64(i), now, now, now, now, payload); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.PruneForecastErrors(ctx, now); err != nil {
		t.Fatal("retention exhausted the scoring budget:", err)
	}
	var count, bytes, oldest int64
	if err := s.db.QueryRow(`SELECT COUNT(*),SUM(length(payload)),MIN(start_ms) FROM forecast_errors`).Scan(&count, &bytes, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != MaxForecastErrors || bytes != MaxForecastErrorBytes || oldest != now-MaxForecastErrors+1 {
		t.Fatalf("retained the wrong prefix: count=%d bytes=%d oldest=%d", count, bytes, oldest)
	}
}

func TestForecastRetentionRechecksConcurrentBudgetChanges(t *testing.T) {
	for _, when := range []string{"between_batches", "inside_snapshot"} {
		t.Run(when, func(t *testing.T) {
			s := openForecastArchive(t)
			now := time.Now().UnixMilli()
			if _, err := s.db.Exec(`CREATE TABLE prune_test(id INTEGER PRIMARY KEY,expiry INTEGER,payload TEXT)`); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 150; i++ {
				if _, err := s.db.Exec(`INSERT INTO prune_test VALUES(?,?,'x')`, i, now); err != nil {
					t.Fatal(err)
				}
			}
			other := s.db
			t.Cleanup(func() { other.Close() })
			var anchors atomic.Int32
			var mutated atomic.Bool
			d := &forecastMutationDriver{hook: func(query string) error {
				ready := when == "between_batches" && strings.Contains(query, "SELECT COALESCE((SELECT rowid") && anchors.Add(1) == 2
				ready = ready || when == "inside_snapshot" && strings.Contains(query, "ORDER BY expiry ASC,id ASC LIMIT")
				if !ready || !mutated.CompareAndSwap(false, true) {
					return nil
				}
				// Delete retained rows and insert newer rows. A stale budget would
				// either over-delete or leave too many rows. Inside a snapshot, the
				// first delete must fail its upgrade and repeat the budget read.
				tx, err := other.Begin()
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err := tx.Exec(`DELETE FROM prune_test WHERE id>76`); err != nil {
					return err
				}
				for i := 151; i <= 250; i++ {
					if _, err := tx.Exec(`INSERT INTO prune_test VALUES(?,?,'x')`, i, now); err != nil {
						return err
					}
				}
				return tx.Commit()
			}}
			name := fmt.Sprintf("forecast-mutation-%d", time.Now().UnixNano())
			sql.Register(name, d)
			var err error
			s.db, err = sql.Open(name, s.mainDBPath+sqlitePragmas)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.pruneForecastRecords(ctx, "prune_test", "expiry DESC,id DESC", "expiry", now, 50, 1000); err != nil {
				t.Fatal(err)
			}
			var count, oldest, newest int
			if err := other.QueryRow(`SELECT COUNT(*),MIN(id),MAX(id) FROM prune_test`).Scan(&count, &oldest, &newest); err != nil {
				t.Fatal(err)
			}
			if !mutated.Load() || count != 50 || oldest != 201 || newest != 250 {
				t.Fatalf("stale retention after concurrent change: mutated=%v count=%d oldest=%d newest=%d", mutated.Load(), count, oldest, newest)
			}
		})
	}
}

type forecastMutationDriver struct {
	base sqlite.Driver
	hook func(string) error
}

func (d *forecastMutationDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &forecastMutationConn{Conn: c, hook: d.hook}, nil
}

type forecastMutationConn struct {
	driver.Conn
	hook func(string) error
}

func (c *forecastMutationConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *forecastMutationConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.hook(query); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *forecastMutationConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *forecastMutationConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *forecastMutationConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }
