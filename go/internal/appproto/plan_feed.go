package appproto

import (
	"sync"
	"time"
)

// PlanSettle is the quiet time after a push. Changes inside it are sent once,
// when it ends, so a burst of replans reaches a phone as the first change at
// once and one follow-up.
const PlanSettle = 100 * time.Millisecond

// PlanFeed pushes the plan to every streaming session the moment what a plan
// reader sees moves: a replan starts and the plan is pending, a plan is
// published, or a replan fails.
//
// The first change goes out at once. Changes inside the settle that follows
// are sent once when it ends, and that send starts another settle. The feed
// runs no periodic timer, only that one wait after a push. One feed serves
// the whole box; each handler joins it when built and leaves when closed.
type PlanFeed struct {
	settle time.Duration

	mu      sync.Mutex
	members map[*Handler]struct{}
	// quiet is set from a push to the end of its settle; dirty says a
	// change came meanwhile.
	quiet bool
	dirty bool
}

// NewPlanFeed builds the box's feed.
func NewPlanFeed(settle time.Duration) *PlanFeed {
	return &PlanFeed{settle: settle, members: map[*Handler]struct{}{}}
}

// Changed is the planner's word that what a plan reader sees has moved.
func (f *PlanFeed) Changed() {
	f.mu.Lock()
	if f.quiet {
		f.dirty = true
		f.mu.Unlock()
		return
	}
	f.quiet = true
	f.mu.Unlock()
	f.push()
	time.AfterFunc(f.settle, f.settled)
}

func (f *PlanFeed) settled() {
	f.mu.Lock()
	if !f.dirty {
		f.quiet = false
		f.mu.Unlock()
		return
	}
	f.dirty = false
	f.mu.Unlock()
	f.push()
	time.AfterFunc(f.settle, f.settled)
}

// push hands each session its plan on a goroutine of its own, so neither the
// planner nor another phone waits on a slow socket. A session sends one plan
// at a time and reads it when it sends, so the newest plan lands last.
func (f *PlanFeed) push() {
	f.mu.Lock()
	members := make([]*Handler, 0, len(f.members))
	for h := range f.members {
		members = append(members, h)
	}
	f.mu.Unlock()
	for _, h := range members {
		go h.pushPlan()
	}
}

func (f *PlanFeed) join(h *Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[h] = struct{}{}
}

func (f *PlanFeed) leave(h *Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.members, h)
}

// pushPlan sends the plan unasked to a phone that streams and may read it,
// unless that phone already has this very plan. The role is read as it
// stands now, the way every privileged request reads it: a phone demoted to
// a viewer stops getting plans at the next one.
func (h *Handler) pushPlan() {
	h.mu.Lock()
	streaming := h.subscribed
	h.mu.Unlock()
	if !streaming || h.cfg.Info.Boot() != nil {
		return
	}
	caller, ok := h.liveCaller()
	if !ok || !caller.Scopes.Has(ScopePlanRead) {
		return
	}
	// A failed send is the session's carrier going away; the carrier ends
	// the session, and this handler leaves the feed when it closes.
	if err := h.writePlan(nil, true); err != nil {
		h.log.Debug("plan push not sent", "err", err)
	}
}
