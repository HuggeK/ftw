package cloudwire

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// The freshness cases from docs/protocol-cloud.md, run against View. Times are
// in ms on one clock unless a case says the box clock is off. Rule N with
// 100 ms in transit unless stated.

const transit = 100

type fakeBox struct {
	t    *testing.T
	key  *ecdsa.PrivateKey
	home string
	boot []byte
	seq  uint64
	// start is the wall time at which uptime was zero. clockLead moves the
	// box's wall clock against the phone's.
	start, clockLead int64
	noClock          bool
}

func newFakeBox(t *testing.T, bootByte byte) *fakeBox {
	key := testKey(t, "freshness box")
	home, _ := homeOf(t, key)
	return &fakeBox{t: t, key: key, home: home, boot: bytes.Repeat([]byte{bootByte}, BootBytes), start: 1_000_000}
}

func (b *fakeBox) header(at int64) Header {
	b.seq++
	h := Header{Seq: b.seq, Boot: b.boot, UptimeMs: at - b.start}
	if !b.noClock {
		w := at + b.clockLead
		h.WallMs = &w
	}
	return h
}

func (b *fakeBox) seal(t string, body any) Envelope {
	env, err := Seal(b.key, b.home, t, body)
	if err != nil {
		b.t.Fatal(err)
	}
	return env
}

// fullLive is one source "m", S = 10 s, read at the given age.
func (b *fakeBox) fullLive(at, age int64, state protocol.SourceState, srcRev uint64) Envelope {
	return b.seal(MsgLive, Live{
		Header: b.header(at), Fields: map[string]int64{"2": 1000, "1": 0},
		FieldSources: map[string][]string{"2": {"m"}},
		Sources:      map[string]Source{"m": {Kind: "driver", State: state, LastOkMs: at - b.start - age, StaleAfterMs: 10_000}},
		SrcRev:       srcRev, Ages: map[string]int64{"m": age},
	})
}

func (b *fakeBox) hb(at, age int64, srcRev, liveSeq, lpSeq uint64) Envelope {
	return b.seal(MsgHB, HB{Header: b.header(at), Clock: ClockNTP, SrcRev: srcRev,
		LiveSeq: liveSeq, LpSeq: lpSeq, Ages: map[string]int64{"m": age}})
}

func (b *fakeBox) view() *View { return NewView(b.home, &b.key.PublicKey) }

func accept(t *testing.T, v *View, e Envelope, arrival int64) {
	t.Helper()
	if err := v.Accept(e, arrival); err != nil {
		t.Fatalf("accept %s at %d: %v", e.T, arrival, err)
	}
}

func wantSource(t *testing.T, v *View, at int64, want protocol.SourceState) {
	t.Helper()
	if got := v.Grade(at).Sources["m"]; got != want {
		t.Fatalf("at %d: source is %s, want %s", at, got, want)
	}
}

// 1. A quiet live source stays live.
func TestFreshnessQuietSourceStaysLive(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	for at := t0 + 5_000; at <= t0+120_000; at += 5_000 {
		accept(t, v, box.hb(at, 4_500, 1, 1, 0), at+transit)
		for probe := at + transit; probe < at+5_000+transit; probe += 500 {
			wantSource(t, v, probe, protocol.SourceLive)
			if f := v.Grade(probe).Fields["2"]; f != protocol.SourceLive {
				t.Fatalf("field 2 is %s", f)
			}
		}
	}
}

// 2. A dead uplink ages on time.
func TestFreshnessDeadUplinkAgesOnTime(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	last := t0 + 5_000
	accept(t, v, box.hb(last, 2_000, 1, 1, 0), last+transit)
	// Send time is the box's wallMs, so t counts from `last`.
	wantSource(t, v, last+8_000, protocol.SourceLive)
	wantSource(t, v, last+8_001, protocol.SourceLagging)
	wantSource(t, v, last+18_000, protocol.SourceLagging)
	wantSource(t, v, last+18_001, protocol.SourceStale)
	wantSource(t, v, last+58_000, protocol.SourceStale)
	wantSource(t, v, last+58_001, protocol.SourceDown)
}

// 3. A replay ages out, and a phone that holds newer drops it.
func TestFreshnessReplayAgesOut(t *testing.T) {
	box := newFakeBox(t, 1)
	t0 := int64(2_000_000)
	live := box.fullLive(t0, 1_000, protocol.SourceLive, 1)
	hb := box.hb(t0+5_000, 2_000, 1, 1, 0)

	fresh := box.view()
	hour := int64(3_600_000)
	accept(t, fresh, live, t0+hour)
	accept(t, fresh, hb, t0+hour+10)
	wantSource(t, fresh, t0+hour+20, protocol.SourceDown)
	if f := fresh.Grade(t0 + hour + 20).Fields["2"]; f != protocol.SourceDown {
		t.Fatalf("replayed field is %s", f)
	}

	current := box.view()
	accept(t, current, live, t0+transit)
	accept(t, current, hb, t0+5_000+transit)
	if err := current.Accept(live, t0+hour); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("replayed live accepted: %v", err)
	}
}

// 4. A withheld table goes stale.
func TestFreshnessWithheldTableGoesStale(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	_ = box.fullLive(t0+3_000, 12_000, protocol.SourceLagging, 2) // dropped by the service
	accept(t, v, box.hb(t0+5_000, 1_000, 2, 2, 0), t0+5_000+transit)
	wantSource(t, v, t0+5_200, protocol.SourceStale)
}

// 5. No anchor, never live.
func TestFreshnessNoAnchorNeverLive(t *testing.T) {
	box := newFakeBox(t, 1)
	box.noClock = true
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	accept(t, v, box.hb(t0+5_000, 500, 1, 1, 0), t0+5_000+transit)
	g := v.Grade(t0 + 5_200)
	if g.Anchored || g.Sources["m"] != protocol.SourceStale || g.Fields["2"] != protocol.SourceStale {
		t.Fatalf("unanchored data graded %+v", g)
	}
}

// 6. A fast box clock loses rule N.
func TestFreshnessFastClockLosesRuleN(t *testing.T) {
	box := newFakeBox(t, 1)
	box.clockLead = 3_000
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	if g := v.Grade(t0 + 200); g.Anchored || g.Sources["m"] == protocol.SourceLive {
		t.Fatalf("fast clock graded %+v", g)
	}
	// The boot stays off rule N even when later messages look fine.
	box.clockLead = 0
	accept(t, v, box.hb(t0+5_000, 500, 1, 1, 0), t0+5_000+transit)
	if g := v.Grade(t0 + 5_200); g.Anchored {
		t.Fatal("rule N came back within the boot")
	}
}

// 7. A small clock lead gains nothing.
func TestFreshnessSmallLeadGainsNothing(t *testing.T) {
	box := newFakeBox(t, 1)
	box.clockLead = 1_500
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 2_000, protocol.SourceLive, 1), t0+transit)
	// Send time is the arrival, t0+100: age 2000 + t reaches S = 10 s at
	// t0+8100, not 1.5 s later.
	wantSource(t, v, t0+8_100, protocol.SourceLive)
	wantSource(t, v, t0+8_101, protocol.SourceLagging)
}

// 8. A changed byte is dropped.
func TestFreshnessChangedByteIsDropped(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	env := box.fullLive(2_000_000, 1_000, protocol.SourceLive, 1)
	env.B = append([]byte(nil), env.B...)
	env.B[len(env.B)-1] ^= 1
	if err := v.Accept(env, 2_000_100); !errors.Is(err, ErrSignature) {
		t.Fatalf("changed byte: %v", err)
	}
}

// 9 and 10. A dropped live or a held-back lp goes stale.
func TestFreshnessDroppedLiveAndLpGoStale(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	accept(t, v, box.seal(MsgLP, LP{Header: box.header(t0 + 10),
		Loadpoints: []Loadpoint{{ID: "lp1", Src: "m", Plugged: true}}}), t0+10+transit)
	heldLive, heldLP := box.seq-1, box.seq
	_ = box.seal(MsgLive, Live{Header: box.header(t0 + 3_000)}) // dropped
	_ = box.seal(MsgLP, LP{Header: box.header(t0 + 3_100)})     // dropped
	accept(t, v, box.hb(t0+5_000, 1_000, 1, heldLive+2, heldLP+2), t0+5_000+transit)
	g := v.Grade(t0 + 5_200)
	if g.Fields["2"] != protocol.SourceStale || g.Fields["1"] != protocol.SourceStale {
		t.Fatalf("fields after a dropped live: %v", g.Fields)
	}
	if g.Loadpoints["lp1"] != protocol.SourceStale {
		t.Fatalf("loadpoint after a held-back lp: %v", g.Loadpoints)
	}
}

// 12. A reboot clears the old table.
func TestFreshnessRebootClearsTheTable(t *testing.T) {
	x := newFakeBox(t, 1)
	v := x.view()
	t0 := int64(2_000_000)
	accept(t, v, x.fullLive(t0, 1_000, protocol.SourceLive, 9), t0+transit)

	y := newFakeBox(t, 2)
	y.start = t0 + 30_000
	_ = y.fullLive(t0+60_000, 1_000, protocol.SourceLive, 3) // dropped
	accept(t, v, y.hb(t0+61_000, 1_000, 3, 1, 0), t0+61_000+transit)
	g := v.Grade(t0 + 61_200)
	if len(g.Sources) != 0 || len(g.Fields) != 0 {
		t.Fatalf("boot X's table survived boot Y: %+v", g)
	}
	// An older message from boot X can no longer take the view back.
	if err := v.Accept(x.hb(t0+1_000, 1_000, 9, 1, 0), t0+61_300); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("old boot replaced the new one: %v", err)
	}
}

// 13. Rule U drift counts against freshness.
func TestFreshnessRuleUDrift(t *testing.T) {
	box := newFakeBox(t, 1)
	box.noClock = true
	v := box.view()
	learned := int64(2_000_000)
	v.LearnOffset(box.boot, learned, learned-box.start)

	tenHours := int64(10 * 3_600_000)
	at := learned + tenHours
	accept(t, v, box.fullLive(at, 0, protocol.SourceLive, 1), at+transit)
	// The margin is 3.6 s: a reading sent with age 0 reaches S = 10 s 3.6 s
	// early.
	wantSource(t, v, at+6_400, protocol.SourceLive)
	wantSource(t, v, at+6_401, protocol.SourceLagging)

	day := learned + RuleULifetimeMs + 1
	if g := v.Grade(day); g.Anchored {
		t.Fatal("rule U outlived its 24 hours")
	}
}

// 14. A snapshot with mixed seq.
func TestFreshnessSnapshotWithMixedSeq(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	box.seq = 4
	prices := box.seal(MsgPrices, Prices{Header: box.header(t0)})
	box.seq = 2989
	live := box.fullLive(t0+10, 1_000, protocol.SourceLive, 1)
	accept(t, v, live, t0+20)
	box.seq = 2999
	accept(t, v, box.fullLive(t0+20, 1_000, protocol.SourceLive, 1), t0+30)
	accept(t, v, prices, t0+40)
}

// 16. The box's state wins.
func TestFreshnessBoxStateWins(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 0, protocol.SourceDown, 1), t0+transit)
	wantSource(t, v, t0+1_000, protocol.SourceDown)
}

func TestHeartbeatIsClamped(t *testing.T) {
	cases := []struct {
		table map[string]Source
		want  int64
	}{
		{nil, 15_000},
		{map[string]Source{"a": {StaleAfterMs: 10_000}}, 5_000},
		{map[string]Source{"a": {StaleAfterMs: 1_000}}, 2_000},
		{map[string]Source{"a": {StaleAfterMs: 120_000}, "b": {StaleAfterMs: 0}}, 15_000},
	}
	for _, c := range cases {
		if got := Heartbeat(c.table); got != c.want {
			t.Errorf("Heartbeat(%v) = %d, want %d", c.table, got, c.want)
		}
	}
}

// 11. The byte budget runs out: the box sends no live and no hb, and the
// data ages exactly as a dead uplink's does.
func TestFreshnessBudgetSpentAgesOnTime(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.fullLive(t0, 2_000, protocol.SourceLive, 1), t0+transit)
	// Nothing more arrives; only events would.
	wantSource(t, v, t0+8_000, protocol.SourceLive)
	wantSource(t, v, t0+8_001, protocol.SourceLagging)
	wantSource(t, v, t0+58_001, protocol.SourceDown)
}

// A message the phone drops changes nothing, even one from a later boot that
// would otherwise have replaced the one held.
func TestADroppedMessageLeavesTheViewAsItWas(t *testing.T) {
	x := newFakeBox(t, 1)
	v := x.view()
	t0 := int64(2_000_000)
	accept(t, v, x.fullLive(t0, 1_000, protocol.SourceLive, 1), t0+transit)
	before := v.Grade(t0 + 1_000)

	y := newFakeBox(t, 2)
	y.start = t0
	broken := y.seal(MsgLive, map[string]any{"seq": 1, "boot": y.boot, "uptimeMs": 5_000,
		"wallMs": t0 + 5_000, "fields": "not a map"})
	if err := v.Accept(broken, t0+5_100); err == nil {
		t.Fatal("an undecodable live was accepted")
	}
	zero := y.seal(MsgHB, HB{Header: Header{Seq: 0, Boot: y.boot, UptimeMs: 5_000, WallMs: &[]int64{t0 + 5_000}[0]}})
	if err := v.Accept(zero, t0+5_100); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("seq 0 from a new boot: %v", err)
	}
	ahead := x.seal(MsgHB, HB{Header: Header{Seq: 0, Boot: x.boot, UptimeMs: 5_000, WallMs: &[]int64{t0 + 60_000}[0]}})
	_ = v.Accept(ahead, t0+5_100) // dropped, and must not turn rule N off
	if got := v.Grade(t0 + 1_000); got.Anchored != before.Anchored || got.Sources["m"] != before.Sources["m"] ||
		got.Fields["2"] != before.Fields["2"] {
		t.Fatalf("a dropped message changed the view: %+v, was %+v", got, before)
	}
}

// A state this phone does not know ranks as never: neither the source nor
// a field that follows it can read as live.
func TestAnUnknownStateIsNeverLive(t *testing.T) {
	box := newFakeBox(t, 1)
	v := box.view()
	t0 := int64(2_000_000)
	accept(t, v, box.seal(MsgLive, Live{
		Header: box.header(t0), Fields: map[string]int64{"2": 1000},
		FieldSources: map[string][]string{"2": {"m"}},
		Sources:      map[string]Source{"m": {Kind: "driver", State: "dormant", StaleAfterMs: 10_000}},
		SrcRev:       1, Ages: map[string]int64{},
	}), t0+transit)
	g := v.Grade(t0 + 200)
	if g.Sources["m"] != protocol.SourceNever || g.Fields["2"] != protocol.SourceNever {
		t.Fatalf("an unknown state graded as %s, field %s", g.Sources["m"], g.Fields["2"])
	}
}
