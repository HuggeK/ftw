package mpc

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type blockingFirstOptimizer struct {
	calls        atomic.Int32
	firstMode    chan Mode
	secondMode   chan Mode
	releaseFirst chan struct{}
}

func (o *blockingFirstOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	call := o.calls.Add(1)
	if call == 1 {
		o.firstMode <- p.Mode
		select {
		case <-o.releaseFirst:
		case <-ctx.Done():
			return Plan{}, ctx.Err()
		}
	} else if call == 2 {
		o.secondMode <- p.Mode
	}

	plan := Optimize(slots, p)
	status := "old-self-consumption"
	if call == 2 {
		status = "new-arbitrage"
	}
	plan.Solver = &SolverInfo{
		Engine: "test", Backend: "blocking", Status: status,
		Formulation: "deterministic",
	}
	return plan, nil
}

func (*blockingFirstOptimizer) Close() error { return nil }

func TestReplanNewestRequestWinsWhenOlderSolveFinishesLast(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	prices := make([]state.PricePoint, 4)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: float64(40 + i*20), TotalOreKwh: float64(90 + i*20),
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	optimizer := &blockingFirstOptimizer{
		firstMode:    make(chan Mode, 1),
		secondMode:   make(chan Mode, 1),
		releaseFirst: make(chan struct{}),
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoCPct: 50, SoCMinPct: 10, SoCMaxPct: 95,
		MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = 4 * time.Hour
	svc.BaseLoad = 500
	svc.Optimizer = optimizer

	type savedDiagnostic struct {
		mode   Mode
		reason string
	}
	saved := make(chan savedDiagnostic, 2)
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		saved <- savedDiagnostic{mode: d.Params.Mode, reason: reason}
		return nil
	}

	oldDone := make(chan *Plan, 1)
	go func() {
		oldDone <- svc.ReplanWithReason(context.Background(), "old-self-consumption")
	}()

	if mode := <-optimizer.firstMode; mode != ModeSelfConsumption {
		t.Fatalf("first solve mode = %q, want %q", mode, ModeSelfConsumption)
	}

	// The second solve starts after the mode change and finishes while the
	// first solve remains blocked.
	svc.SetMode(context.Background(), ModeArbitrage)
	if mode := <-optimizer.secondMode; mode != ModeArbitrage {
		t.Fatalf("second solve mode = %q, want %q", mode, ModeArbitrage)
	}

	published := svc.Latest()
	if published == nil || published.Solver == nil || published.Solver.Status != "new-arbitrage" {
		t.Fatalf("newer plan was not published: %+v", published)
	}
	if got := <-saved; got.mode != ModeArbitrage || got.reason != "mode_changed" {
		t.Fatalf("saved diagnostic = %+v, want arbitrage/mode_changed", got)
	}

	close(optimizer.releaseFirst)
	if got := <-oldDone; got != published {
		t.Fatalf("superseded caller returned an unpublished plan: got=%p published=%p", got, published)
	}

	svc.mu.RLock()
	lastMode := svc.lastParams.Mode
	lastReason := svc.lastReason
	lastGeneration := svc.latestReplanGeneration
	svc.mu.RUnlock()
	if lastMode != ModeArbitrage || lastReason != "mode_changed" || lastGeneration != 2 {
		t.Fatalf("published state = mode %q reason %q generation %d", lastMode, lastReason, lastGeneration)
	}
	if latest := svc.Latest(); latest != published {
		t.Fatal("older solve replaced the newer published plan")
	}
	if d := svc.Diagnose(); d == nil || d.Params.Mode != ModeArbitrage || d.LastReason != "mode_changed" {
		t.Fatalf("diagnostic was replaced by the older solve: %+v", d)
	}
	select {
	case extra := <-saved:
		t.Fatalf("superseded solve persisted a diagnostic: %+v", extra)
	default:
	}
}
