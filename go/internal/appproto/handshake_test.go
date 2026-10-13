package appproto

import (
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/control"
)

func TestHelloNegotiatesDownToTheClient(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoMax},
		App:   AppInfo{Build: "test", UA: "pwa"},
	})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoMax)
	}
	if ok.Mode != BoxModeFull {
		t.Fatalf("mode = %q, want full", ok.Mode)
	}
	if ok.Hint != "" {
		t.Fatalf("hint = %q, want none", ok.Hint)
	}
}

func TestHelloCanCarryTheFirstSubscription(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoMax},
		App:   AppInfo{Build: "test", UA: "pwa"},
		Sub:   &Sub{Bucket: 512, Hz: 1},
	})

	frames := rec.snapshot()
	if len(frames) != 2 || frames[0].env.T != MsgHelloOK || frames[1].env.T != MsgSnap {
		t.Fatalf("fast hello sent %s, want hello_ok then snap", rec.types())
	}
	ok := body[HelloOK](t, frames[0])
	if !ok.Subscribed {
		t.Fatal("hello_ok did not acknowledge the folded subscription")
	}
	if !h.Subscribed() {
		t.Fatal("folded subscription did not start telemetry")
	}
}

func TestHelloWithoutAFoldedSubscriptionKeepsTheOldFlow(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Subscribed || h.Subscribed() || rec.has(MsgSnap) {
		t.Fatal("ordinary hello subscribed before the old client sent sub")
	}
}

// An app too old for the box degrades instead of dying. A hard version wall is
// a white screen for everyone whose service worker pinned an old bundle.
func TestHelloFromTooOldAnAppGetsFloorNotAnError(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoFloor},
		App:   AppInfo{Build: "ancient", UA: "pwa"},
	})

	if rec.has(MsgError) {
		t.Fatal("box answered an old app with an error")
	}

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoFloor {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoFloor)
	}
	if ok.Mode != BoxModeFloor {
		t.Fatalf("mode = %q, want floor", ok.Mode)
	}
	if ok.Hint != HintAppUpdate {
		t.Fatalf("hint = %q, want %q", ok.Hint, HintAppUpdate)
	}
	if len(ok.Caps) != 1 || ok.Caps[0] != CapStatusCore {
		t.Fatalf("floor caps = %v, want only %q", ok.Caps, CapStatusCore)
	}
	// The catalogue still goes out: field 1 is an index into it, so a floor
	// session that omitted it could not name the mode it is running.
	if len(ok.Modes) != len(control.ModeCatalog()) {
		t.Fatalf("floor mode catalogue has %d entries, want %d", len(ok.Modes), len(control.ModeCatalog()))
	}
}

// hello_ok carries a capability list and a mode catalogue, both of which vary
// in size with what the box supports. Lane 0 exists so that its frames do not
// vary in size with anything, so this reply belongs on bulk.
func TestHelloOKTravelsOnTheBulkLane(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})

	f := rec.only(t, MsgHelloOK)
	if f.lane != LaneBulk {
		t.Fatalf("hello_ok went out on lane %d, want %d (bulk)", f.lane, LaneBulk)
	}
	if f.size <= 512 {
		t.Fatalf("hello_ok is %d bytes; if it fits lane 0 this test proves nothing", f.size)
	}
}

// The catalogue is control's, verbatim. A hand-written list here would let the
// app offer a mode the box does not have, or hide one it does.
func TestHelloOKModesComeFromTheControlCatalogue(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	cat := control.ModeCatalog()
	if len(ok.Modes) != len(cat) {
		t.Fatalf("catalogue has %d entries, wire has %d", len(cat), len(ok.Modes))
	}
	for i, m := range cat {
		got := ok.Modes[i]
		if got.Key != string(m.Key) || got.Label != m.Label ||
			got.Tooltip != m.Tooltip || got.Tier != string(m.Tier) {
			t.Fatalf("mode %d: wire %+v, catalogue %+v", i, got, m)
		}
	}
}

func TestHelloOKReportsBootProgress(t *testing.T) {
	h, box, rec, _ := newRig(t)
	eta := int64(90_000)
	box.boot = &BootProgress{Phase: BootPhaseVacuum, Pct: 40, EtaMs: &eta}

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: 0, Max: ProtoMax},
		Sub:   &Sub{Bucket: 512, Hz: 1},
	})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	if ok.Mode != BoxModeBooting {
		t.Fatalf("mode = %q, want booting", ok.Mode)
	}
	if ok.Boot == nil || ok.Boot.Phase != BootPhaseVacuum || ok.Boot.Pct != 40 {
		t.Fatalf("boot = %+v", ok.Boot)
	}
	if ok.Subscribed || rec.has(MsgSnap) {
		t.Fatal("booting box acknowledged a subscription it cannot serve")
	}
}

// Ages are uptime deltas. A Pi reads 1970 until NTP answers, so a wall clock
// on the wire without its provenance would make every freshness claim wrong by
// decades exactly when someone looks after a power cut.
func TestHelloOKCarriesUptimeAndClockProvenance(t *testing.T) {
	h, _, rec, clock := newRig(t)
	clock.uptimeMs = 123_456

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))

	if ok.Clock.UptimeMs != 123_456 {
		t.Fatalf("uptimeMs = %d, want 123456", ok.Clock.UptimeMs)
	}
	if ok.Clock.Source != "ntp" {
		t.Fatalf("clock source = %q", ok.Clock.Source)
	}
}

func TestCapsHashIsOrderIndependentAndSetSensitive(t *testing.T) {
	a := capsHash([]string{CapStatusCore, CapCmdLease})
	b := capsHash([]string{CapCmdLease, CapStatusCore})
	if a != b {
		t.Fatalf("the same set in a different order hashed differently: %q vs %q", a, b)
	}
	c := capsHash([]string{CapStatusCore})
	if a == c {
		t.Fatal("a different capability set hashed the same")
	}
}

func TestNewRefusesACapabilityNotInTheRegistry(t *testing.T) {
	_, box, rec, clock := newRig(t)
	_, err := New(Config{
		Clock: clock, Site: box, Info: box, Modes: box, Plans: box,
		Codec: testCodec{}, Sender: rec,
		Caller: ownerCaller(), Grants: newGrants(),
		Caps: []string{"status.core", "status.kore"},
	})
	if err == nil {
		t.Fatal("a hand-typed capability name was accepted")
	}
}

// Both directions ignore what they do not recognise. That single rule is what
// lets a newer app talk to an older box and the reverse.
func TestUnknownMessageTypeIsIgnoredWithoutARequestId(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, "der.enumerate", nil, map[string]any{"x": 1})
	if len(rec.frames) != 0 {
		t.Fatalf("unsolicited unknown type produced %s", rec.types())
	}
}

func TestUnknownMessageTypeWithARequestIdIsAnswered(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, "der.enumerate", ptrU32(9), map[string]any{"x": 1})

	f := rec.only(t, MsgError)
	if f.env.ID == nil || *f.env.ID != 9 {
		t.Fatalf("error did not echo the request id: %+v", f.env.ID)
	}
	e := body[ErrorBody](t, f)
	if e.Code != ErrUnknownOp {
		t.Fatalf("code = %q, want %q", e.Code, ErrUnknownOp)
	}
}

// A newer app sends keys this build has never heard of. Dropping them is the
// point; refusing the message would be the version wall by another route.
func TestUnknownBodyKeysAreIgnored(t *testing.T) {
	h, _, rec, _ := newRig(t)

	deliver(t, h, MsgHello, nil, map[string]any{
		"proto":     map[string]any{"min": 0, "max": ProtoMax, "preferred": 3},
		"app":       map[string]any{"build": "future", "ua": "pwa", "channel": "beta"},
		"locales":   []string{"sv"},
		"telepathy": true,
	})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax {
		t.Fatalf("proto = %d, want %d", ok.Proto, ProtoMax)
	}
}

func TestSubWhileBootingIsRefusedWithARetryableCode(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.boot = &BootProgress{Phase: BootPhaseMigrate, Pct: 10}

	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	e := body[ErrorBody](t, rec.only(t, MsgError))
	if e.Code != ErrBooting {
		t.Fatalf("code = %q, want %q", e.Code, ErrBooting)
	}
	if !e.Retryable {
		t.Fatal("E_BOOTING must be retryable; the box is about to be ready")
	}
	if h.Subscribed() {
		t.Fatal("a booting box accepted a subscription")
	}
}

// Every role reads FTW Cloud, so every role learns the box's key, home id,
// boot and names. The hook answers nil while FTW Cloud is off, and hello_ok
// then says nothing about it.
func TestHelloOKCarriesTheCloudSessionForEveryRole(t *testing.T) {
	session := &CloudSession{
		PK: append([]byte{4}, make([]byte, 64)...), Home: "abcdefghijklmnopqrstuvwxyz",
		Boot:       make([]byte, 16),
		Sources:    map[string]string{"AAAAAAAAAAA": "meter.p1"},
		Loadpoints: map[string]string{"BBBBBBBBBBB": "garage"},
	}
	for _, c := range []struct {
		name   string
		caller apiauth.Caller
		hook   func() *CloudSession
		want   *CloudSession
	}{
		{"owner", ownerCaller(), func() *CloudSession { return session }, session},
		{"viewer", viewerCaller(), func() *CloudSession { return session }, session},
		{"FTW Cloud off", ownerCaller(), func() *CloudSession { return nil }, nil},
		{"no uplink", viewerCaller(), nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, rec, _ := newRig(t)
			h.cfg.Caller, h.cfg.Cloud = c.caller, c.hook
			deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
			ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
			if !reflect.DeepEqual(ok.Cloud, c.want) {
				t.Fatalf("cloud = %+v, want %+v", ok.Cloud, c.want)
			}
		})
	}
}

// An app too old for full mode gets the frozen subset, and FTW Cloud is not in
// it.
func TestFloorModeLeavesTheCloudSessionOut(t *testing.T) {
	h, _, rec, _ := newRig(t)
	h.cfg.Cloud = func() *CloudSession {
		t.Fatal("floor mode asked for FTW Cloud's session information")
		return nil
	}
	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoFloor}})
	if ok := body[HelloOK](t, rec.only(t, MsgHelloOK)); ok.Cloud != nil {
		t.Fatalf("floor hello_ok carries %+v", ok.Cloud)
	}
}

// The field rides under one key, so an older app skips it whole.
func TestTheCloudSessionWireNames(t *testing.T) {
	raw, err := Marshal(HelloOK{Cloud: &CloudSession{PK: []byte{4}, Home: "h", Boot: []byte{1},
		Sources: map[string]string{"s": "d"}, Loadpoints: map[string]string{"l": "p"}}})
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	cloud, ok := generic["cloud"].(map[any]any)
	if !ok {
		t.Fatalf("hello_ok keys %v", generic)
	}
	for _, k := range []string{"pk", "home", "boot", "sources", "loadpoints"} {
		if _, ok := cloud[k]; !ok {
			t.Fatalf("cloud has no %q: %v", k, cloud)
		}
	}
	if len(cloud) != 5 {
		t.Fatalf("cloud has %d keys: %v", len(cloud), cloud)
	}
}
