package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// Delay the real SQLite rollback after database/sql has marked its Tx done.
// A cancelled write must retain ownership until that rollback has finished.
func TestHistoryCancelledWriteWaitsForRollback(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: base, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := s.history.Close(); err != nil {
		t.Fatal(err)
	}
	d := &slowHistoryRollbackDriver{started: make(chan struct{}), release: make(chan struct{})}
	name := fmt.Sprintf("history-rollback-%d", time.Now().UnixNano())
	sql.Register(name, d)
	var err error
	s.history, err = sql.Open(name, s.historyPath+strings.Replace(sqlitePragmas, "synchronous(NORMAL)", "synchronous(FULL)", 1))
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(func() { close(d.release) })
	batch := historyBatch{id: "cancelled", hash: "same-payload", payload: historyPayload{Samples: []Sample{{Driver: "meter", Metric: "grid_w", TsMs: base + 1000, Value: 20}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	d.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := s.recordHistoryBatches(ctx, []historyBatch{batch}, 0)
		done <- err
	}()
	select {
	case <-d.started:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled transaction did not start rollback")
	}
	returnedBeforeRollback := false
	var writeErr error
	select {
	case writeErr = <-done:
		returnedBeforeRollback = true
	case <-time.After(50 * time.Millisecond):
	}
	// The rollback still owns SQLite's writer. Retrying a deferred transaction
	// after reading a receipt returns BUSY immediately, despite busy_timeout.
	if returnedBeforeRollback {
		retryCtx, stop := context.WithTimeout(context.Background(), time.Second)
		_, retryErr := s.recordHistoryBatches(retryCtx, []historyBatch{batch}, 0)
		stop()
		t.Logf("retry while rollback is pending: %v", retryErr)
	}
	release.Do(func() { close(d.release) })
	if !returnedBeforeRollback {
		select {
		case writeErr = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("write did not finish after rollback")
		}
	}
	if returnedBeforeRollback {
		t.Fatal("history write returned and released its mutex before SQLite rollback finished")
	}
	if !errors.Is(writeErr, context.DeadlineExceeded) || !strings.Contains(writeErr.Error(), "history tick:") {
		t.Fatalf("missing deadline phase: %v", writeErr)
	}
	if _, err := s.recordHistoryBatches(context.Background(), []historyBatch{batch}, 0); err != nil {
		t.Fatal("retry after rollback:", err)
	}
	if _, err := s.recordHistoryBatches(context.Background(), []historyBatch{batch}, 0); err != nil {
		t.Fatal("identical receipt retry:", err)
	}
	var n int
	var sum float64
	if err := s.history.QueryRow(`SELECT n,sum_value FROM ts_buckets WHERE resolution_ms=? AND start_ms=?`, HistoryResolutionMS, base).Scan(&n, &sum); err != nil || n != 1 || sum != 20 {
		t.Fatalf("rollback/retry changed telemetry: n=%d sum=%v err=%v", n, sum, err)
	}
}

type slowHistoryRollbackDriver struct {
	queryNeedle string
	execNeedle  string
	blocked     chan struct{}
	base        sqlite.Driver
	armed       atomic.Bool
	started     chan struct{}
	release     chan struct{}
}

func (d *slowHistoryRollbackDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &slowHistoryRollbackConn{Conn: c, d: d}, nil
}

type slowHistoryRollbackConn struct {
	driver.Conn
	d *slowHistoryRollbackDriver
}

func (c *slowHistoryRollbackConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &slowHistoryRollbackTx{Tx: tx, d: c.d}, nil
}

func (c *slowHistoryRollbackConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	target := c.d.queryNeedle == "" && isHistoryTestWrite(query)
	if c.d.execNeedle != "" {
		target = strings.Contains(query, c.d.execNeedle)
	}
	if err == nil && ctx.Done() != nil && target && c.d.armed.CompareAndSwap(true, false) {
		if c.d.blocked != nil {
			close(c.d.blocked)
		}
		<-ctx.Done()
		// Let database/sql's cancellation goroutine claim the Tx before the
		// driver returns, while this operation still owns the connection lock.
		time.Sleep(20 * time.Millisecond)
		return r, ctx.Err()
	}
	return r, err
}

func (c *slowHistoryRollbackConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.d.queryNeedle != "" && strings.Contains(query, c.d.queryNeedle) && c.d.armed.CompareAndSwap(true, false) {
		close(c.d.blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *slowHistoryRollbackConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *slowHistoryRollbackConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type slowHistoryRollbackTx struct {
	driver.Tx
	d *slowHistoryRollbackDriver
}

func (tx *slowHistoryRollbackTx) Rollback() error {
	if tx.d.release != nil {
		select {
		case <-tx.d.started:
		default:
			close(tx.d.started)
			<-tx.d.release
		}
	}
	return tx.Tx.Rollback()
}

func isHistoryTestWrite(query string) bool {
	q := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(q, "INSERT ") || strings.HasPrefix(q, "UPDATE ") || strings.HasPrefix(q, "DELETE ")
}

func TestHistoryMaintenanceWaitsForCancelledRollback(t *testing.T) {
	for _, kind := range []string{"archive", "backfill", "completion"} {
		t.Run(kind, func(t *testing.T) {
			s := freshStore(t)
			if kind == "backfill" {
				if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "pv_w", TsMs: seriesHourMs, Value: 1200}}); err != nil {
					t.Fatal(err)
				}
			}
			for _, query := range []string{`DELETE FROM ts_series_hour`, `DELETE FROM history_migrations WHERE name='ts-series-hour-v1'`} {
				if _, err := s.history.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.history.Close(); err != nil {
				t.Fatal(err)
			}
			d := &slowHistoryRollbackDriver{started: make(chan struct{}), release: make(chan struct{})}
			name := fmt.Sprintf("maintenance-rollback-%s-%d", kind, time.Now().UnixNano())
			sql.Register(name, d)
			var err error
			s.history, err = sql.Open(name, s.historyPath+strings.Replace(sqlitePragmas, "synchronous(NORMAL)", "synchronous(FULL)", 1))
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			defer release.Do(func() { close(d.release) })
			run := func(ctx context.Context) error {
				switch kind {
				case "archive":
					return s.tryArchiveBatch(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `INSERT INTO history_site_cursor(key,value) VALUES ('rollback-test',1)`)
						return err
					})
				case "backfill":
					_, err := s.backfillSeriesHoursSnapshot(ctx, 64)
					return err
				default:
					return s.ensureSeriesHours(ctx)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			d.armed.Store(true)
			done := make(chan error, 1)
			go func() { done <- run(ctx) }()
			select {
			case <-d.started:
			case err := <-done:
				t.Fatalf("maintenance ended before rollback: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("maintenance did not start rollback")
			}
			waitCtx, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
			lockErr := lockContext(waitCtx, s.historyWriteMu.TryLock)
			stop()
			if lockErr == nil {
				s.historyWriteMu.Unlock()
			}
			release.Do(func() { close(d.release) })
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("maintenance did not finish after rollback")
			}
			if lockErr == nil {
				t.Fatal("maintenance released the writer mutex before SQLite rollback finished")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled maintenance: %v", err)
			}
			if err := run(context.Background()); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
			// Check every pooled connection: archive must restore its zero busy timeout.
			var conns []*sql.Conn
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			for i := 0; i < 4; i++ {
				c, err := s.history.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, c)
				var busy int
				if err := c.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
					t.Fatalf("busy timeout leaked: %d %v", busy, err)
				}
			}
		})
	}
}
