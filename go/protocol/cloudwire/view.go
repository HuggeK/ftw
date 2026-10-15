package cloudwire

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/srcfl/ftw/go/protocol"
)

// The phone's half of the freshness rules, as a reference.
//
// The service cannot sign, so it cannot make data look fresher than the box
// graded it; it can only delay, drop or replay, and each of those must age the
// data or mark it stale on the phone. View is that phone-side logic in Go: the
// vectors in docs/protocol-cloud.md run against it here, and the Swift port
// replays the same cases. Nothing in the box or the service uses it.

// Timing rules from the protocol's Freshness section.
const (
	// RuleNLeadMs is how far ahead of the phone clock a box's wallMs may be
	// before the phone stops trusting the box clock for that boot.
	RuleNLeadMs = 2_000
	// RuleULifetimeMs is how long an uptime offset learned over Noise stays
	// usable.
	RuleULifetimeMs = 24 * 60 * 60 * 1000
	// ruleUDriftDivisor sets the margin rule U adds to every age: 0.01 % of
	// the time since the offset was learned.
	ruleUDriftDivisor = 10_000
)

// ErrNotNewer is a message the phone already has something newer than.
var ErrNotNewer = errors.New("cloudwire: older than what the phone holds")

// View is what a phone holds for one home.
type View struct {
	home string
	key  *ecdsa.PublicKey

	boot    []byte
	seq     map[string]uint64
	noRuleN bool
	newest  int64 // latest anchored send time held, phone clock

	table    map[string]Source
	tableRev uint64
	fieldSrc map[string][]string
	live     *Live
	lp       *LP
	m        *anchor

	offsets map[string]uptimeOffset
}

// anchor is M: the newest anchored live or hb.
type anchor struct {
	seq      uint64
	hb       bool
	boot     []byte
	wallSend int64 // send time by rule N, or -1
	uptimeMs int64
	srcRev   uint64
	liveSeq  uint64
	lpSeq    uint64
	ages     map[string]int64
}

type uptimeOffset struct {
	offsetMs    int64
	learnedAtMs int64
}

// NewView starts an empty view of one home, with the box key the phone
// learned over Noise. Never a key from the service.
func NewView(home string, boxKey *ecdsa.PublicKey) *View {
	return &View{home: home, key: boxKey, seq: map[string]uint64{}, offsets: map[string]uptimeOffset{}}
}

// LearnOffset records rule U's anchor from a Noise session with the box:
// phone time minus box uptime, the smallest seen per boot.
func (v *View) LearnOffset(boot []byte, phoneMs, uptimeMs int64) {
	k := hex.EncodeToString(boot)
	off := phoneMs - uptimeMs
	if cur, ok := v.offsets[k]; ok && cur.offsetMs <= off {
		cur.learnedAtMs = phoneMs
		v.offsets[k] = cur
		return
	}
	v.offsets[k] = uptimeOffset{offsetMs: off, learnedAtMs: phoneMs}
}

// Accept takes one box envelope that arrived at arrivalMs on the phone clock.
// It returns an error, and changes nothing, for a message it drops.
func (v *View) Accept(e Envelope, arrivalMs int64) error {
	if err := Verify(v.key, v.home, e); err != nil {
		return err
	}
	if _, ok := ClassScope[e.T]; !ok {
		return fmt.Errorf("cloudwire: %q is not a class a phone reads", e.T)
	}
	var h Header
	if err := Unmarshal(e.B, &h); err != nil {
		return err
	}

	// Decode the class body before anything changes, so a message that is
	// dropped leaves the view as it was.
	var (
		live *Live
		lp   *LP
		hb   *HB
	)
	switch e.T {
	case MsgLive:
		live = new(Live)
		if err := Unmarshal(e.B, live); err != nil {
			return err
		}
	case MsgLP:
		lp = new(LP)
		if err := Unmarshal(e.B, lp); err != nil {
			return err
		}
	case MsgHB:
		hb = new(HB)
		if err := Unmarshal(e.B, hb); err != nil {
			return err
		}
	}

	lead := h.WallMs != nil && *h.WallMs > arrivalMs+RuleNLeadMs
	newBoot := !bytes.Equal(h.Boot, v.boot)
	if newBoot {
		// Another boot replaces this one only with an anchored message sent
		// after the newest one held, so a replay of an older boot cannot
		// wipe a newer one.
		send, ok := v.sendTime(h.Boot, wallSend(h, arrivalMs, lead), h.UptimeMs, arrivalMs)
		if (v.boot != nil && (!ok || send <= v.newest)) || h.Seq == 0 {
			return ErrNotNewer
		}
		v.reset(h.Boot)
	} else if h.Seq <= v.seq[e.T] {
		return ErrNotNewer
	}
	if lead {
		v.noRuleN = true
	}
	ws := wallSend(h, arrivalMs, v.noRuleN)
	send, anchored := v.sendTime(h.Boot, ws, h.UptimeMs, arrivalMs)

	switch {
	case live != nil:
		v.live = live
		if live.Sources != nil {
			v.table, v.tableRev, v.fieldSrc = live.Sources, live.SrcRev, live.FieldSources
		}
		if anchored {
			v.setM(&anchor{seq: h.Seq, boot: h.Boot, wallSend: ws, uptimeMs: h.UptimeMs,
				srcRev: live.SrcRev, liveSeq: h.Seq, ages: live.Ages})
		}
	case lp != nil:
		v.lp = lp
	case hb != nil:
		if anchored {
			v.setM(&anchor{seq: h.Seq, hb: true, boot: h.Boot, wallSend: ws, uptimeMs: h.UptimeMs,
				srcRev: hb.SrcRev, liveSeq: hb.LiveSeq, lpSeq: hb.LpSeq, ages: hb.Ages})
		}
	}
	v.seq[e.T] = h.Seq
	if anchored && send > v.newest {
		v.newest = send
	}
	return nil
}

func (v *View) setM(a *anchor) {
	if v.m == nil || a.seq > v.m.seq {
		v.m = a
	}
}

func (v *View) reset(boot []byte) {
	v.boot = append([]byte(nil), boot...)
	v.seq = map[string]uint64{}
	v.noRuleN = false
	v.table, v.tableRev, v.fieldSrc = nil, 0, nil
	v.live, v.lp, v.m = nil, nil, nil
}

// wallSend is rule N: the earlier of wallMs and arrival, or -1 when the box
// sent no wall time or rule N is off for this boot.
func wallSend(h Header, arrivalMs int64, off bool) int64 {
	if h.WallMs == nil || off {
		return -1
	}
	return min(*h.WallMs, arrivalMs)
}

// sendTime picks the earlier of rule N and rule U. ok is false when neither
// applies at nowMs.
func (v *View) sendTime(boot []byte, wall, uptimeMs, nowMs int64) (int64, bool) {
	send, ok := wall, wall >= 0
	if u, uok := v.ruleU(boot, uptimeMs, nowMs); uok && (!ok || u < send) {
		send, ok = u, true
	}
	return send, ok
}

func (v *View) ruleU(boot []byte, uptimeMs, nowMs int64) (int64, bool) {
	off, ok := v.offsets[hex.EncodeToString(boot)]
	if !ok || nowMs-off.learnedAtMs > RuleULifetimeMs {
		return 0, false
	}
	return off.offsetMs + uptimeMs, true
}

// Grades is how a phone would show what it holds at one moment.
type Grades struct {
	// Anchored is false when no send time can be trusted: the phone shows
	// "cloud, age unverified" and never live.
	Anchored bool
	// Sources is each source's effective state.
	Sources map[string]protocol.SourceState
	// Fields is each live field's state, keyed by field id.
	Fields map[string]protocol.SourceState
	// Loadpoints is each loadpoint's state, keyed by its opaque id.
	Loadpoints map[string]protocol.SourceState
}

// Grade applies the protocol's state rules at nowMs.
func (v *View) Grade(nowMs int64) Grades {
	g := Grades{
		Sources:    map[string]protocol.SourceState{},
		Fields:     map[string]protocol.SourceState{},
		Loadpoints: map[string]protocol.SourceState{},
	}
	var t int64
	if v.m != nil {
		wall := v.m.wallSend
		if v.noRuleN {
			wall = -1
		}
		send, ok := v.sendTime(v.m.boot, wall, v.m.uptimeMs, nowMs)
		if ok {
			g.Anchored = true
			t = max(0, nowMs-send)
			if u, uok := v.ruleU(v.m.boot, v.m.uptimeMs, nowMs); uok && u == send {
				off := v.offsets[hex.EncodeToString(v.m.boot)]
				t += (nowMs - off.learnedAtMs) / ruleUDriftDivisor
			}
		}
	}

	tableBehind := g.Anchored && v.m.srcRev > v.tableRev
	for id, s := range v.table {
		state := worse(s.State, protocol.SourceLive)
		switch {
		case !g.Anchored || tableBehind:
			state = worse(state, protocol.SourceStale)
		default:
			if age, ok := v.m.ages[id]; ok {
				state = worse(state, ladder(age+t, s.StaleAfterMs))
			}
		}
		g.Sources[id] = state
	}

	if v.live != nil {
		fieldsBehind := !g.Anchored || (v.m.hb && v.m.liveSeq > v.seq[MsgLive])
		for f := range v.live.Fields {
			state := protocol.SourceLive
			srcs := v.fieldSrc[f]
			if len(srcs) == 0 {
				if t > 2*v.heartbeatMs() {
					state = protocol.SourceStale
				}
			}
			for _, s := range srcs {
				st, ok := g.Sources[s]
				if !ok {
					st = protocol.SourceStale
				}
				state = worse(state, st)
			}
			if fieldsBehind {
				state = worse(state, protocol.SourceStale)
			}
			g.Fields[f] = state
		}
	}

	if v.lp != nil {
		lpBehind := !g.Anchored || (v.m.hb && v.m.lpSeq > v.seq[MsgLP])
		for _, lp := range v.lp.Loadpoints {
			state, ok := g.Sources[lp.Src]
			if !ok {
				state = protocol.SourceStale
			}
			if lpBehind {
				state = worse(state, protocol.SourceStale)
			}
			g.Loadpoints[lp.ID] = state
		}
	}
	return g
}

// heartbeatMs is H: half the smallest staleAfterMs, between 2 and 15 s.
func (v *View) heartbeatMs() int64 {
	return Heartbeat(v.table)
}

// Heartbeat is H for a source table: clamp(smallest staleAfterMs ÷ 2, 2 s,
// 15 s), and 15 s when the table holds no source with a cadence.
func Heartbeat(table map[string]Source) int64 {
	smallest := int64(-1)
	for _, s := range table {
		if s.StaleAfterMs > 0 && (smallest < 0 || s.StaleAfterMs < smallest) {
			smallest = s.StaleAfterMs
		}
	}
	if smallest < 0 {
		return 15_000
	}
	return min(max(smallest/2, 2_000), 15_000)
}

// ladder grades an age the way the box grades its own sources.
func ladder(age, staleAfter int64) protocol.SourceState {
	switch {
	case staleAfter <= 0:
		return protocol.SourceNever
	case age <= staleAfter:
		return protocol.SourceLive
	case age <= 2*staleAfter:
		return protocol.SourceLagging
	case age <= 6*staleAfter:
		return protocol.SourceStale
	default:
		return protocol.SourceDown
	}
}

var stateRank = map[protocol.SourceState]int{
	protocol.SourceLive: 0, protocol.SourceLagging: 1, protocol.SourceStale: 2,
	protocol.SourceDown: 3, protocol.SourceNever: 4,
}

// worse is the worse of two states. A state this phone does not know ranks
// as never, whichever side it is on, so it can never read as live.
func worse(a, b protocol.SourceState) protocol.SourceState {
	if _, ok := stateRank[a]; !ok {
		a = protocol.SourceNever
	}
	if _, ok := stateRank[b]; !ok {
		b = protocol.SourceNever
	}
	if stateRank[b] > stateRank[a] {
		return b
	}
	return a
}
