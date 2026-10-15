package appproto

import (
	"crypto/rand"
	"encoding/base64"
	"sort"
	"strings"
	"sync"

	"github.com/srcfl/ftw/go/protocol"
)

// AgentState is what the box keeps about agents beyond one session: the
// durable changes waiting for an owner, and each agent's rate. One per box,
// like the command log: an agent that reconnects meets its own minute and
// its own requests again.
//
// Requests live in memory. A restart drops them, and the agent asks again;
// docs/agents.md says so.
type AgentState struct {
	mu       sync.Mutex
	requests map[string]*agentRequest
	cmds     map[string][]int64
	reads    map[string][]int64
	newID    func() string
	// listeners are the open agent sessions, by agent id, told how their
	// requests end.
	listeners map[string]map[int]func(protocol.AgentDecided)
	nextL     int
	// ended are requests that expired since listeners last heard.
	ended []endedReq
	// folds counts an agent's reads per message and minute, for the audit.
	folds map[foldKey]*fold
	// holds is when each agent last set a hold on each charger.
	holds map[string]int64
}

type endedReq struct {
	agent string
	d     protocol.AgentDecided
}

type foldKey struct{ agent, what, by, result string }

type fold struct {
	row    protocol.AuditRow
	minute int64
}

type agentRequest struct {
	// Request holds the args as the box resolved them: the owner's view,
	// which the owner signs.
	protocol.Request
	// sent is the args as the agent sent them, with its own ids: the
	// agent's view.
	sent map[string]any
	// endedAtMs is when it stopped pending, or 0 while it waits.
	endedAtMs int64
}

// NewAgentState makes an empty one.
func NewAgentState() *AgentState {
	return &AgentState{
		requests: map[string]*agentRequest{},
		cmds:     map[string][]int64{},
		reads:    map[string][]int64{},
		newID:    newRequestID,
	}
}

func newRequestID() string {
	b := make([]byte, protocol.ReqIDBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

const rateWindowMs = 60_000

// allow takes one call from an agent's rolling minute, or says how long
// until there is room. now is box uptime.
func allow(window map[string][]int64, agent string, limit int, nowMs int64) (bool, int64) {
	if limit <= 0 {
		// A grant written before a default was filled in still has one.
		limit = protocol.PerMinDefault
	}
	kept := window[agent][:0]
	for _, at := range window[agent] {
		if nowMs-at < rateWindowMs {
			kept = append(kept, at)
		}
	}
	window[agent] = kept
	if len(kept) >= limit {
		return false, rateWindowMs - (nowMs - kept[0])
	}
	window[agent] = append(kept, nowMs)
	return true, 0
}

// allowCmd counts a cmd or an agent.request against the grant's perMin.
func (s *AgentState) allowCmd(agent string, perMin uint64, nowMs int64) (bool, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return allow(s.cmds, agent, int(perMin), nowMs)
}

// allowRead counts a read against the box's own limit.
func (s *AgentState) allowRead(agent string, nowMs int64) (bool, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return allow(s.reads, agent, protocol.ReadsPerMin, nowMs)
}

// pruneLocked ends requests past their life and forgets those ended long
// enough ago. live says whether an agent still has a row; a request from one
// that does not is gone with it. now is wall clock.
func (s *AgentState) pruneLocked(nowMs int64, live func(string) bool) {
	for id, r := range s.requests {
		switch {
		case live != nil && !live(r.Agent):
			delete(s.requests, id)
		case r.endedAtMs == 0 && nowMs >= r.ExpiresAtMs:
			r.State = protocol.RequestExpired
			r.endedAtMs = nowMs
			s.ended = append(s.ended, endedReq{r.Agent, protocol.AgentDecided{ReqID: r.ReqID, State: protocol.RequestExpired}})
		case r.endedAtMs != 0 && nowMs-r.endedAtMs >= protocol.RequestKeptMs:
			delete(s.requests, id)
		}
	}
}

// add stores a new pending request: args resolved for the owner, sent as
// the agent sent them.
func (s *AgentState) add(agent, op string, args, sent map[string]any, why string, nowMs int64) (protocol.Request, *protocol.Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(nowMs, nil)
	mine, all := 0, 0
	for _, r := range s.requests {
		if r.State != protocol.RequestPending {
			continue
		}
		all++
		if r.Agent == agent {
			mine++
		}
	}
	if mine >= protocol.PendingPerAgent {
		return protocol.Request{}, protocol.Refuse(protocol.ErrCapExceeded, map[string]any{"cap": "pending", "max": protocol.PendingPerAgent})
	}
	if all >= protocol.PendingPerBox {
		return protocol.Request{}, protocol.Refuse(protocol.ErrCapExceeded, map[string]any{"cap": "pending", "max": protocol.PendingPerBox})
	}
	r := &agentRequest{Request: protocol.Request{
		ReqID: s.newID(), Agent: agent, Op: op, Args: args, Why: why,
		AtMs: nowMs, ExpiresAtMs: nowMs + protocol.RequestTTLMs, State: protocol.RequestPending,
	}, sent: sent}
	s.requests[r.ReqID] = r
	return r.Request, nil
}

// list returns the requests of one agent, or of all when agent is empty,
// oldest first. An agent's own list shows the args as it sent them.
func (s *AgentState) list(agent string, nowMs int64, live func(string) bool) []protocol.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(nowMs, live)
	out := []protocol.Request{}
	for _, r := range s.requests {
		switch {
		case agent == "":
			out = append(out, r.Request)
		case r.Agent == agent:
			mine := r.Request
			mine.Args = r.sent
			out = append(out, mine)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].AtMs != out[b].AtMs {
			return out[a].AtMs < out[b].AtMs
		}
		return out[a].ReqID < out[b].ReqID
	})
	return out
}

// pending returns a request that still waits for an answer.
func (s *AgentState) pending(reqID string, nowMs int64, live func(string) bool) (protocol.Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(nowMs, live)
	r := s.requests[reqID]
	if r == nil || r.State != protocol.RequestPending {
		return protocol.Request{}, false
	}
	return r.Request, true
}

// decide ends a pending request with state. It fails if another answer
// came first.
func (s *AgentState) decide(reqID, state string, nowMs int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.requests[reqID]
	if r == nil || r.State != protocol.RequestPending {
		return false
	}
	r.State = state
	r.endedAtMs = nowMs
	return true
}

// Forget forgets an agent that is gone: its requests and its minute.
func (s *AgentState) Forget(agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.requests {
		if r.Agent == agent {
			delete(s.requests, id)
		}
	}
	delete(s.cmds, agent)
	delete(s.reads, agent)
	for key := range s.holds {
		if strings.HasPrefix(key, agent+"\x00") {
			delete(s.holds, key)
		}
	}
}

// subscribe registers an open agent session to hear how its requests end.
func (s *AgentState) subscribe(agent string, fn func(protocol.AgentDecided)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listeners == nil {
		s.listeners = map[string]map[int]func(protocol.AgentDecided){}
	}
	if s.listeners[agent] == nil {
		s.listeners[agent] = map[int]func(protocol.AgentDecided){}
	}
	s.nextL++
	n := s.nextL
	s.listeners[agent][n] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners[agent], n)
	}
}

// tell sends how a request ended to its agent's open sessions, and any
// expiries the last prune found. Called without the mutex.
func (s *AgentState) tell(agent string, d protocol.AgentDecided) {
	s.mu.Lock()
	type note struct {
		fn func(protocol.AgentDecided)
		d  protocol.AgentDecided
	}
	var notes []note
	if agent != "" {
		for _, fn := range s.listeners[agent] {
			notes = append(notes, note{fn, d})
		}
	}
	for _, e := range s.ended {
		for _, fn := range s.listeners[e.agent] {
			notes = append(notes, note{fn, e.d})
		}
	}
	s.ended = nil
	s.mu.Unlock()
	for _, n := range notes {
		n.fn(n.d)
	}
}

// foldRead counts one read in its agent's minute. It returns the row of a
// minute that has ended, to be written, or nil.
func (s *AgentState) foldRead(row protocol.AuditRow, nowMs int64) *protocol.AuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folds == nil {
		s.folds = map[foldKey]*fold{}
	}
	key := foldKey{row.Actor, row.What, row.By, row.Result}
	minute := nowMs / rateWindowMs
	f := s.folds[key]
	switch {
	case f == nil:
		row.Count = 1
		s.folds[key] = &fold{row: row, minute: minute}
		return nil
	case f.minute == minute:
		f.row.Count++
		return nil
	}
	done := f.row
	row.Count = 1
	s.folds[key] = &fold{row: row, minute: minute}
	return &done
}

// flushFolds returns every folded minute that has ended by nowMs, to be
// written, and forgets them.
func (s *AgentState) flushFolds(nowMs int64) []protocol.AuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.AuditRow
	for key, f := range s.folds {
		if f.minute < nowMs/rateWindowMs {
			out = append(out, f.row)
			delete(s.folds, key)
		}
	}
	return out
}

// FlushAudit writes the folded reads of every minute that has ended. The box
// runs it now and then, so a quiet agent's last minute is not lost.
func (s *AgentState) FlushAudit(log AuditLog, nowMs int64) {
	if log == nil {
		return
	}
	for _, row := range s.flushFolds(nowMs) {
		log.Append(row)
	}
}

// holdGapMs is the least time between two holds an agent sets on one
// charger, so an agent cannot flap a charger between nothing and full power.
const holdGapMs = 30_000

// allowHold records an agent's hold on a charger, or says how long until
// the next may come. now is box uptime.
func (s *AgentState) allowHold(agent, charger string, nowMs int64) (bool, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]int64{}
	}
	key := agent + "\x00" + charger
	if last, ok := s.holds[key]; ok && nowMs-last < holdGapMs {
		return false, holdGapMs - (nowMs - last)
	}
	s.holds[key] = nowMs
	return true, 0
}

// Sweep ends requests past their life and tells their agents, without
// waiting for a call to prune them. The box runs it now and then.
func (s *AgentState) Sweep(nowMs int64) {
	s.mu.Lock()
	s.pruneLocked(nowMs, nil)
	s.mu.Unlock()
	s.tell("", protocol.AgentDecided{})
}
