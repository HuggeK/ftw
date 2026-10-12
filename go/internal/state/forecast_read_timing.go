package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Report failed reads before cursor/connection cleanup. Record no payload,
// bound SQL arguments, cursor IDs or successful-read logs.
type forecastReadTiming struct {
	historyWriteTiming
	operation     string
	budgetEntryMS int64
	rowCount      int
	conn          *sql.Conn
	rows          *sql.Rows
}

func newForecastReadTiming(ctx context.Context, operation string) forecastReadTiming {
	t := forecastReadTiming{
		historyWriteTiming: newHistoryWriteTiming("connection"),
		operation:          operation,
		budgetEntryMS:      -1, // Caller supplied no deadline.
	}
	if deadline, ok := ctx.Deadline(); ok {
		t.budgetEntryMS = max(0, time.Until(deadline).Milliseconds())
	}
	return t
}

func (t *forecastReadTiming) finish(err *error) {
	if *err != nil {
		*err = fmt.Errorf("%w [read=%s budget_entry_ms=%d rows=%d]",
			t.failure(*err), t.operation, t.budgetEntryMS, t.rowCount)
	}
	if t.rows != nil {
		t.rows.Close()
	}
	if t.conn != nil {
		t.conn.Close()
	}
}
