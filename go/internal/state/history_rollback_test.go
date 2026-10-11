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
	base    sqlite.Driver
	armed   atomic.Bool
	started chan struct{}
	release chan struct{}
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
	if err == nil && ctx.Done() != nil && c.d.armed.CompareAndSwap(true, false) {
		<-ctx.Done()
		// Let database/sql's cancellation goroutine claim the Tx before the
		// driver returns, while this operation still owns the connection lock.
		time.Sleep(20 * time.Millisecond)
		return r, ctx.Err()
	}
	return r, err
}

func (c *slowHistoryRollbackConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
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
