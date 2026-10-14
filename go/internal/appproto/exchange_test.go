package appproto

import (
	"fmt"
	"testing"
)

// newExchangeRig is a watch's exchange session over the EV rig's box.
func newExchangeRig(t *testing.T) (*Handler, *fakeLoadpoints, *fakeBox, *recorder) {
	t.Helper()
	h, lp, box, rec, _ := newEVRig(t)
	h.cfg.Exchange = true
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = newWatchGrants()
	lp.plugged, lp.soc, lp.socKnown, lp.powerW = true, 0.62, true, 7360
	return h, lp, box, rec
}

// hello_ok on the exchange says what a watch needs, in a bulk bucket, and
// carries the controlRev a command expects.
func TestAnExchangeHelloNamesTheRevAndNoCatalogue(t *testing.T) {
	h, _, box, rec := newExchangeRig(t)
	h.cfg.Cloud = func() *CloudSession { return &CloudSession{Home: "home-1", PK: make([]byte, 65), Boot: make([]byte, 16)} }

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: ProtoMin, Max: ProtoMax}})
	frame := rec.only(t, MsgHelloOK)
	if frame.lane != LaneBulk || frame.size != 1024 {
		t.Fatalf("hello_ok went on lane %d in %d B, want the 1 KiB bulk bucket", frame.lane, frame.size)
	}
	ok := body[ExchangeHelloOK](t, frame)
	if ok.Rev != box.snap.ControlRev || ok.Role != watchCaller().Role || ok.Cloud == nil || ok.Cloud.Home != "home-1" {
		t.Fatalf("hello_ok = %+v", ok)
	}
	keys := body[map[string]any](t, frame)
	for _, stream := range []string{"modes", "caps", "capsHash", "box"} {
		if _, ok := keys[stream]; ok {
			t.Fatalf("the exchange hello_ok carries %q, which only the stream needs", stream)
		}
	}
}

// The relay answers a watch with at most 4 KiB, so hello_ok never takes the
// 4 KiB bucket. A house too big for 1 KiB leaves FTW Cloud out, and the watch
// reads now instead.
func TestAnExchangeHelloTooBigForTheRelayLeavesCloudOut(t *testing.T) {
	h, _, _, rec := newExchangeRig(t)
	sources := map[string]string{}
	for i := 0; i < 80; i++ {
		sources[fmt.Sprintf("opaque-source-%03d", i)] = fmt.Sprintf("a-rather-long-driver-name-%03d", i)
	}
	h.cfg.Cloud = func() *CloudSession { return &CloudSession{Home: "home-1", Sources: sources} }

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: ProtoMin, Max: ProtoMax}})
	frame := rec.only(t, MsgHelloOK)
	if frame.size > ExchangeHelloMaxBucket {
		t.Fatalf("hello_ok took %d B", frame.size)
	}
	if ok := body[ExchangeHelloOK](t, frame); ok.Cloud != nil {
		t.Fatal("hello_ok kept FTW Cloud past what the relay can carry")
	}
}

// An applied command on the exchange is answered by its ack and result and
// nothing after them: the plan the stream pushes would be a third frame.
func TestAnExchangeCommandPushesNoPlan(t *testing.T) {
	h, _, _, rec := newExchangeRig(t)
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "x-hold", holdArgs(), 7, 200_000))

	if got := rec.types(); got != fmt.Sprint([]string{MsgCmdAck, MsgCmdResult}) {
		t.Fatalf("an applied command sent %s", got)
	}
	if res := body[CmdResult](t, rec.only(t, MsgCmdResult)); res.State != CmdApplied {
		t.Fatalf("the hold ended %q", res.State)
	}
}

// now is one lane 0 frame. What does not fit is left out and flagged, never
// sent in a bigger frame.
func TestNowFitsLaneZeroOrSaysWhatItLeftOut(t *testing.T) {
	h, lp, _, rec := newExchangeRig(t)
	deliver(t, h, MsgNow, nil, struct{}{})
	frame := rec.only(t, MsgNow)
	if frame.lane != LaneControl || frame.size != 512 || frame.flags&FlagTrunc != 0 {
		t.Fatalf("now went on lane %d in %d B with flags %d", frame.lane, frame.size, frame.flags)
	}
	now := body[Now](t, frame)
	if len(now.Loadpoints) != 1 || now.Loadpoints[0].SocPermille == nil || *now.Loadpoints[0].SocPermille != 620 {
		t.Fatalf("now = %+v", now)
	}
	if _, ok := now.Sources[fidKey(FidSrcGrid)]; !ok {
		t.Fatalf("now ages no grid source: %+v", now.Sources)
	}
	rec.reset()

	for i := 0; i < 40; i++ {
		lp.ids[fmt.Sprintf("charger-with-a-long-name-%02d", i)] = true
	}
	deliver(t, h, MsgNow, nil, struct{}{})
	frame = rec.only(t, MsgNow)
	if frame.size != 512 || frame.flags&FlagTrunc == 0 {
		t.Fatalf("an overfull now went in %d B with flags %d", frame.size, frame.flags)
	}
	now = body[Now](t, frame)
	if len(now.Loadpoints) == 0 || len(now.Fields) == 0 {
		t.Fatalf("a trimmed now kept %d chargers and %d fields", len(now.Loadpoints), len(now.Fields))
	}
}

// A removed watch's read is refused and its session ended, as a command is.
func TestARemovedWatchReadsNothing(t *testing.T) {
	h, _, _, rec := newExchangeRig(t)
	h.cfg.Grants.(*fakeGrants).revoke()

	deliver(t, h, MsgNow, nil, struct{}{})
	if rec.has(MsgNow) {
		t.Fatal("a removed watch read the house")
	}
	if e := body[ErrorBody](t, rec.only(t, MsgError)); e.Code != ErrGrantRevoked {
		t.Fatalf("the refusal is %s", e.Code)
	}
	if !rec.has(MsgSessionTerminate) {
		t.Fatal("the session went on")
	}
}

// The pad is a lane 0 tick of the session's one size.
func TestThePadIsALaneZeroTick(t *testing.T) {
	h, _, _, _ := newExchangeRig(t)
	raw, err := h.PadFrame()
	if err != nil {
		t.Fatal(err)
	}
	f, err := testCodec{}.DecodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 512 || f.Lane != LaneControl || f.Envelope.T != MsgTick {
		t.Fatalf("the pad is %d B, lane %d, %q", len(raw), f.Lane, f.Envelope.T)
	}
}
