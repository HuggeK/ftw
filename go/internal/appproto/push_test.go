package appproto

import (
	"fmt"
	"testing"
)

// A session through FTW Cloud hears every poll the moment it lands: a delta
// of what changed, or a tick, so the frame's timing shows when the box read
// its devices and never which reading moved.
func TestPushSendsAFrameOnEveryPoll(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if f := rec.only(t, MsgTick); f.lane != LaneControl || f.size != 512 {
		t.Fatalf("a poll that changed nothing sent lane %d, %d bytes; want a 512-byte tick", f.lane, f.size)
	}

	rec.reset()
	box.snap.GridW = 1450
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	f := rec.only(t, MsgDelta)
	if f.lane != LaneControl || f.size != 512 {
		t.Fatalf("pushed delta: lane %d, %d bytes; want lane 0 in its 512-byte bucket", f.lane, f.size)
	}
	d := body[Delta](t, f)
	if len(d.Fields) != 1 || d.Fields[fidKey(FidGridW)] != 1450 {
		t.Fatalf("pushed delta carries %v, want only grid_w 1450", d.Fields)
	}

	rec.reset()
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[tick]" {
		t.Fatalf("the same reading again sent %s, want a tick", got)
	}
}

// Hidden, the phone asked for one frame in five seconds. A poll does not
// break that; the beat carries what changed when the five seconds are up.
func TestPushWaitsForTheBeatWhileHidden(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 0.2})

	box.snap.GridW = 1450
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	clock.uptimeMs += 4_999
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[]" {
		t.Fatalf("hidden session sent %s within its five seconds", got)
	}
	clock.uptimeMs++
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if d := body[Delta](t, rec.only(t, MsgDelta)); d.Fields[fidKey(FidGridW)] != 1450 {
		t.Fatalf("the hidden beat carries %v, want grid_w 1450", d.Fields)
	}
}

// Silence on lane 0 means the session is gone, so a quiet house still gets
// a tick once per subscribed period, counted from the last frame of any kind.
func TestBeatTicksOnlyAfterAQuietPeriod(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	clock.uptimeMs += 999
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[]" {
		t.Fatalf("beat %d ms after the snapshot sent %s", 999, got)
	}
	clock.uptimeMs++
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	tick := rec.only(t, MsgTick)
	if tick.size != 512 {
		t.Fatalf("beat tick is %d bytes, want the 512-byte bucket", tick.size)
	}

	// A pushed delta resets the quiet period.
	rec.reset()
	clock.uptimeMs += 500
	box.snap.GridW = 1450
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	clock.uptimeMs += 999
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[delta]" {
		t.Fatalf("got %s; a beat within a second of the delta must send nothing", got)
	}
	clock.uptimeMs++
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[delta tick]" {
		t.Fatalf("got %s, want the delta and then one tick", got)
	}

	// One sequence across pushes and beats, with no gap for a poll that
	// changed nothing.
	frames := rec.snapshot()
	if d, k := body[Delta](t, frames[0]), body[Tick](t, frames[1]); d.Seq != 2 || k.Seq != 3 {
		t.Fatalf("seq %d then %d, want 2 then 3", d.Seq, k.Seq)
	}
}

// A frame the carrier lost is not news the phone has: the next frame
// carries it again, and the quiet period runs from the last frame that left.
func TestADroppedFrameIsSentAgain(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	rec.fail = fmt.Errorf("tun: %w", ErrDropped)
	box.snap.GridW = 1450
	if err := h.Push(); err != nil {
		t.Fatalf("a dropped frame failed the session: %v", err)
	}
	rec.fail = nil
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if d := body[Delta](t, rec.only(t, MsgDelta)); d.Fields[fidKey(FidGridW)] != 1450 {
		t.Fatalf("the next frame carries %v, want grid_w 1450 again", d.Fields)
	}

	rec.reset()
	clock.uptimeMs += 1000
	rec.fail = ErrDropped
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	rec.fail = nil
	if err := h.Beat(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[tick]" {
		t.Fatalf("after a dropped beat the next beat sent %s, want a tick", got)
	}
}

// A snapshot the carrier lost goes again with the first frame, before it.
func TestADroppedSnapshotGoesAgainFirst(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: ProtoMin, Max: ProtoMax}})
	rec.reset()
	rec.fail = ErrDropped
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})
	if !h.Subscribed() {
		t.Fatal("a dropped snapshot left the session unsubscribed")
	}
	rec.fail = nil
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[snap tick]" {
		t.Fatalf("got %s, want the snapshot again and then lane 0's frame", got)
	}
}

// inLine records which frames went in line with lane 0.
type inLine struct {
	*recorder
	types []string
}

func (s *inLine) SendInLine(frame []byte) error {
	f, err := testCodec{}.DecodeFrame(frame)
	if err != nil {
		return err
	}
	s.types = append(s.types, f.Envelope.T)
	return s.Send(frame)
}

// Through FTW Cloud bulk waits behind lane 0. The snapshot must not: a delta
// that overtook it would be applied, then overwritten with older values.
func TestTheSnapshotGoesInLineWithLaneZero(t *testing.T) {
	h, box, rec, _ := newRig(t)
	sender := &inLine{recorder: rec}
	h.cfg.Sender = sender
	subscribe(t, h, rec)
	box.snap.ControlRev++
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sender.types) != "[snap snap]" {
		t.Fatalf("in line: %v; want both snapshots and nothing else", sender.types)
	}
}

// Six drivers' source table does not fit a 512-byte lane 0 frame. When one
// goes stale the phone must still hear it, so the table goes in a snapshot,
// once, and the frame after it is a tick.
func TestSixDriversSourcesThatDoNotFitGoInASnapshot(t *testing.T) {
	h, box, rec, clock := newRig(t)
	box.snap.Sources = nil
	for _, name := range []string{"Sungrow SH10RT hybrid inverter", "Sungrow SBR battery stack",
		"Easee Home charger in the carport", "P1 meter in the hall cupboard", "Ferroamp EnergyHub XL",
		"Shelly Pro 3EM on the heat pump"} {
		box.snap.Sources = append(box.snap.Sources, Source{
			ID: "driver." + name[:6], Kind: "driver", Name: name, LastOkUptimeMs: 60_000, StaleAfterMs: 5_000,
		})
	}
	subscribe(t, h, rec)

	// One driver stops answering: its state moves to lagging.
	box.snap.Sources[3].LastOkUptimeMs = 54_000
	clock.uptimeMs += 1_000
	probe := Delta{Fields: map[string]int64{}, Sources: wireSources(box.snap, clock.uptimeMs), DispatchBlockedBy: []string{}}
	if _, flags, _ := h.fitDelta(&probe, 512); flags&FlagTrunc == 0 {
		t.Fatal("six drivers' sources fit a 512-byte delta; the test needs a table that does not")
	}
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[snap tick]" {
		t.Fatalf("got %s, want the table in a snapshot and lane 0's tick", got)
	}
	snap := body[Snap](t, rec.only(t, MsgSnap))
	if src := snap.Sources["driver.P1 met"]; src.State != SourceLagging {
		t.Fatalf("the snapshot says the quiet meter is %q, want lagging", src.State)
	}

	rec.reset()
	if err := h.Push(); err != nil {
		t.Fatal(err)
	}
	if got := rec.types(); got != "[tick]" {
		t.Fatalf("the next poll sent %s; the table was already told", got)
	}
}
