package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestForecastReadReportsConnectionWaitAndKeepsDeadline(t *testing.T) {
	readers := map[string]func(*Store, context.Context) error{
		"score_page": func(s *Store, ctx context.Context) error {
			_, _, err := s.LoadForecastScorePage(ctx, 0, 10, ForecastIssueCursor{}, 4)
			return err
		},
		"errors": func(s *Store, ctx context.Context) error {
			_, err := s.LoadForecastErrors(ctx, 0, 10, false)
			return err
		},
		"observations": func(s *Store, ctx context.Context) error {
			_, err := s.LoadForecastObservations(ctx, 0, 10)
			return err
		},
	}
	for name, load := range readers {
		t.Run(name, func(t *testing.T) {
			s := openForecastArchive(t)
			s.db.SetMaxOpenConns(1)
			blocker, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			started := time.Now()
			err = load(s, ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost caller deadline: %v", err)
			}
			for _, field := range []string{"phase=connection", "connection_ms=", "budget_entry_ms=", "read=" + name, "rows=0"} {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("connection wait lacks %s: %v", field, err)
				}
			}
			if time.Since(started) > time.Second {
				t.Fatal("reader extended caller deadline")
			}
			blocker.Close()
			retry, retryCancel := context.WithTimeout(context.Background(), time.Second)
			defer retryCancel()
			if err := load(s, retry); err != nil {
				t.Fatalf("retry after connection release: %v", err)
			}
			// A successful read must release the only connection before a write.
			if _, err := s.db.ExecContext(retry, "DELETE FROM forecast_observations"); err != nil {
				t.Fatalf("read held the connection after return: %v", err)
			}
		})
	}
}

func TestForecastReadSeparatesQueryAndDecodeFailureWithoutPayload(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		s := openForecastArchive(t)
		if _, err := s.db.Exec(`DROP TABLE forecast_issues`); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.LoadForecastScorePage(context.Background(), 0, 10, ForecastIssueCursor{}, 4)
		if err == nil || !strings.Contains(err.Error(), "phase=query") || !strings.Contains(err.Error(), "rows=0") {
			t.Fatalf("missing-table query phase: %v", err)
		}
	})
	t.Run("decode", func(t *testing.T) {
		s := openForecastArchive(t)
		if _, err := s.db.Exec(`INSERT INTO forecast_observations
 (start_ms,end_ms,available_at_ms,config_version,payload) VALUES(1,2,2,'cfg','private-payload-marker')`); err != nil {
			t.Fatal(err)
		}
		_, err := s.LoadForecastObservations(context.Background(), 0, 10)
		if err == nil || !strings.Contains(err.Error(), "phase=decode") || !strings.Contains(err.Error(), "rows=1") {
			t.Fatalf("corrupt-row decode phase: %v", err)
		}
		if strings.Contains(err.Error(), "private-payload-marker") || strings.Contains(err.Error(), "cfg") {
			t.Fatalf("diagnostic exposed row data: %v", err)
		}
	})
}

func TestForecastReadReportsBudgetAlreadySpentAtEntry(t *testing.T) {
	s := openForecastArchive(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, _, err := s.LoadForecastScorePage(ctx, 0, 10, ForecastIssueCursor{}, 4)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "budget_entry_ms=0") {
		t.Fatalf("already-spent caller budget not identified: %v", err)
	}
}
