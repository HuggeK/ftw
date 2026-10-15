package mpc

import (
	"context"
	"sync/atomic"
	"testing"
)

// A phone shows the plan the box holds, so the box says each time that moves:
// a replan starting, a published plan, and a newest replan that ends without
// one. A request that a newer one replaced says nothing when it ends; the
// newer one will. Every listener hears every change.
func TestOnPlanChangeHearsEveryChangeAReaderSees(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	var a, b atomic.Int32
	svc.OnPlanChange(func() { a.Add(1) })
	svc.OnPlanChange(func() { b.Add(1) })

	// Started, then published.
	if svc.Replan(context.Background()) == nil || a.Load() != 2 {
		t.Fatalf("published plan: %d calls, want 2", a.Load())
	}

	// Started, then failed.
	svc.Zone = "no-prices"
	svc.RequestReplan("saved-goal")
	waitForRequestedReplans(t, svc)
	if got := a.Load(); got != 4 || !svc.PlanSnapshot().Outdated {
		t.Fatalf("failed replan: %d calls, want 4", got)
	}

	older := svc.beginReplan(context.Background(), "older")
	newer := svc.beginReplan(context.Background(), "newer")
	if got := a.Load(); got != 6 || !svc.PlanSnapshot().Pending {
		t.Fatalf("two starts: %d calls, want 6", got)
	}
	svc.finishReplan(older)
	if got := a.Load(); got != 6 {
		t.Fatalf("a replaced request spoke: %d calls", got)
	}
	svc.finishReplan(newer)
	if got := a.Load(); got != 7 {
		t.Fatalf("the newest request ending unpublished: %d calls, want 7", got)
	}
	if a.Load() != b.Load() {
		t.Fatalf("listeners heard %d and %d changes", a.Load(), b.Load())
	}
}
