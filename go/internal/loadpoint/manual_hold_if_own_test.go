package loadpoint

import (
	"testing"
	"time"
)

// An agent's hold is set and cleared in one step with the check, so an
// owner's hold that lands in between still wins.
func TestAnAgentsHoldNeverReplacesAnOwners(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	now := time.Now()
	agent := ManualHold{PowerW: 3680, ExpiresAt: now.Add(time.Minute), SetBy: "agent:a1"}
	if !c.SetManualHoldIfOwn("garage", agent, now) {
		t.Fatal("an agent's hold on a free charger")
	}
	if !c.SetManualHoldIfOwn("garage", agent, now) {
		t.Fatal("an agent replacing its own hold")
	}
	c.SetManualHold("garage", ManualHold{PowerW: 0, Persistent: true, SetBy: "app:owner"})
	if c.SetManualHoldIfOwn("garage", agent, now) {
		t.Error("an agent's hold replaced an owner's")
	}
	if c.ClearManualHoldIfOwn("garage", "agent:a1", now) {
		t.Error("an agent cleared an owner's hold")
	}
	if h, held := c.GetManualHold("garage", now); !held || h.SetBy != "app:owner" {
		t.Fatalf("the owner's hold is gone: %+v %v", h, held)
	}
	c.ClearManualHold("garage")
	c.SetManualHold("garage", ManualHold{PowerW: 1000, ExpiresAt: now.Add(time.Second), SetBy: "app:owner"})
	if !c.SetManualHoldIfOwn("garage", agent, now.Add(2*time.Second)) {
		t.Error("an owner's hold that has ended still blocks")
	}
	if !c.ClearManualHoldIfOwn("garage", "agent:a1", now) {
		t.Error("an agent could not clear its own hold")
	}
}
