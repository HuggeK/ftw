package cloudwire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// Push vectors: APNs payloads as the service makes them, the phone's clock
// when each arrives, and what the phone must make of it. ReadAlert is the
// reference; the phone's notification extension replays the same file.
//
//	go test ./cloudwire -run TestPushVectors -update
//
// Signatures are stored, as in cloud-vectors.json, so an update that only
// adds a case leaves the others' bytes alone.

const pushVectorsPath = "testdata/push-vectors.json"

type pushVectorFile struct {
	// Keys are the box keys the phone holds, by home.
	Keys  []pushVectorKey  `json:"keys"`
	Cases []pushVectorCase `json:"cases"`
}

type pushVectorKey struct {
	Home string `json:"home"`
	PK   string `json:"pk"`
}

type pushVectorCase struct {
	Name string `json:"name"`
	// Payload is the APNs payload, exactly as the service sends it.
	Payload string `json:"payload"`
	// Sig is the event's stored signature, hex, empty when there is no
	// event or it is unsigned.
	Sig string `json:"sig"`
	// Offset is rule U's anchor the phone holds, absent when it holds none.
	Offset *pushVectorOffset `json:"offset,omitempty"`
	NowMs  int64             `json:"nowMs"`
	Want   pushVectorOut     `json:"want"`
}

type pushVectorOffset struct {
	Home        string `json:"home"`
	Boot        string `json:"boot"`
	OffsetMs    int64  `json:"offsetMs"`
	LearnedAtMs int64  `json:"learnedAtMs"`
}

type pushVectorOut struct {
	Verdict string            `json:"verdict"`
	Home    string            `json:"home"`
	Kind    string            `json:"kind,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	Src     string            `json:"src,omitempty"`
	LP      string            `json:"lp,omitempty"`
	Boot    string            `json:"boot,omitempty"`
	Seq     uint64            `json:"seq,omitempty"`
	AtMs    int64             `json:"atMs,omitempty"`
}

func TestPushVectors(t *testing.T) {
	box := testKey(t, "ftw.cloud vector box")
	other := testKey(t, "ftw.cloud vector box two")
	home, boxPK := homeOf(t, box)
	otherHome, otherPK := homeOf(t, other)
	grantKey := bytes.Repeat([]byte{0x77}, GrantKeyBytes)
	charger := SourceID(grantKey, "easee")
	garage := LoadpointID(grantKey, "garage")
	boot := bytes.Repeat([]byte{0xb0}, BootBytes)
	wall := int64(1_791_820_800_000)
	at := func(uptime int64) *int64 { return i64(wall + uptime - 60_000) }

	var stored pushVectorFile
	if raw, err := os.ReadFile(pushVectorsPath); err == nil {
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
	} else if !*update {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	storedSig := func(i int, name string) []byte {
		if i >= len(stored.Cases) || stored.Cases[i].Name != name {
			if *update {
				return nil
			}
			t.Fatalf("push vector %d is not %q in %s; regenerate with -update", i, name, pushVectorsPath)
		}
		sig, _ := hex.DecodeString(stored.Cases[i].Sig)
		return sig
	}

	connected := Event{
		Header:  Header{Seq: 9, Boot: boot, UptimeMs: 71_000, WallMs: at(71_000)},
		EventID: "Q2ZrMjR0dzVh", Ts: i64(*at(71_000) - 400), Kind: protocol.PushChargingConnected,
		Severity: SeverityInfo, Args: map[string]string{}, Src: charger, LP: garage,
	}
	interrupted := Event{
		Header:  Header{Seq: 10, Boot: boot, UptimeMs: 72_000, WallMs: at(72_000)},
		EventID: "aW50cnB0ZDAx", Ts: i64(*at(72_000) - 300), Kind: protocol.PushChargingInterrupted,
		Severity: SeverityWarning, Args: map[string]string{}, Src: charger, LP: garage,
	}
	noClock := connected
	noClock.WallMs, noClock.Ts, noClock.Seq = nil, nil, 12
	ahead := connected
	ahead.WallMs, ahead.Ts, ahead.Seq = at(80_000), nil, 13
	unknown := connected
	unknown.Kind, unknown.Seq = "charging.someday", 14
	goal := Event{
		Header:  Header{Seq: 15, Boot: boot, UptimeMs: 74_000, WallMs: at(74_000)},
		EventID: "Z29hbHJpc2sx", Ts: i64(*at(74_000) - 100), Kind: protocol.PushChargingGoalAtRisk,
		Severity: SeverityWarning, Args: map[string]string{"target": "80", "by": "07:00"}, LP: garage,
	}
	signedAsOther := connected
	signedAsOther.Seq = 16

	// Rule U: the phone learned phone time minus box uptime an hour before
	// the event, so the anchor carries 0.01 % of that hour as margin.
	learned := *at(71_000) - 60*60*1000
	ruleU := &Offset{Boot: boot, OffsetMs: wall - 60_000, LearnedAtMs: learned}
	otherBoot := &Offset{Boot: bytes.Repeat([]byte{0xb1}, BootBytes), OffsetMs: ruleU.OffsetMs, LearnedAtMs: learned}
	dayOld := &Offset{Boot: boot, OffsetMs: ruleU.OffsetMs, LearnedAtMs: fresh0(connected) - RuleULifetimeMs - 1}
	byRuleU := func(e Event, nowMs int64) pushVectorOut {
		return pushVectorOut{Verdict: AlertEvent, Home: home, Kind: e.Kind, Args: e.Args, Src: e.Src, LP: e.LP,
			Boot: hex.EncodeToString(e.Boot), Seq: e.Seq,
			AtMs: ruleU.OffsetMs + e.UptimeMs - (nowMs-learned)/ruleUDriftDivisor}
	}

	type spec struct {
		name    string
		key     *ecdsa.PrivateKey // signer; nil for no event
		signFor string            // home the box signs for
		event   *Event
		alert   Alert // Home and Kind; Event is filled in
		mutate  func(env []byte) []byte
		offset  *Offset // rule U's anchor the phone holds for home
		nowMs   int64
		want    pushVectorOut
	}
	fresh := fresh0
	event := func(e Event) pushVectorOut {
		at := min(*e.WallMs, fresh(e))
		if e.Ts != nil {
			at = min(at, *e.Ts)
		}
		return pushVectorOut{Verdict: AlertEvent, Home: home, Kind: e.Kind, Args: e.Args, Src: e.Src, LP: e.LP,
			Boot: hex.EncodeToString(e.Boot), Seq: e.Seq, AtMs: at}
	}
	generic := pushVectorOut{Verdict: AlertGeneric, Home: home}
	specs := []spec{
		{name: "connected, fresh", key: box, signFor: home, event: &connected, alert: Alert{Home: home},
			nowMs: fresh(connected), want: event(connected)},
		{name: "interrupted, fresh", key: box, signFor: home, event: &interrupted, alert: Alert{Home: home},
			nowMs: fresh(interrupted), want: event(interrupted)},
		{name: "goal at risk, fresh", key: box, signFor: home, event: &goal, alert: Alert{Home: home},
			nowMs: fresh(goal), want: event(goal)},
		{name: "one byte of the body changed", key: box, signFor: home, event: &connected, alert: Alert{Home: home},
			mutate: func(env []byte) []byte {
				out := bytes.Clone(env)
				i := bytes.Index(out, []byte("charging.connected"))
				out[i+len("charging.")] ^= 0x01
				return out
			},
			nowMs: fresh(connected), want: generic},
		{name: "named for another home the phone holds", key: box, signFor: home, event: &connected,
			alert: Alert{Home: otherHome}, nowMs: fresh(connected),
			want: pushVectorOut{Verdict: AlertGeneric, Home: otherHome}},
		{name: "signed by another box for this home", key: other, signFor: home, event: &signedAsOther,
			alert: Alert{Home: home}, nowMs: fresh(signedAsOther), want: generic},
		{name: "two hours old", key: box, signFor: home, event: &connected, alert: Alert{Home: home},
			nowMs: *connected.WallMs + 2*60*60*1000, want: generic},
		{name: "ten minutes old is still fresh", key: box, signFor: home, event: &interrupted, alert: Alert{Home: home},
			nowMs: *interrupted.Ts + AlertMaxAgeMs,
			want: func() pushVectorOut {
				o := event(interrupted)
				o.AtMs = *interrupted.Ts
				return o
			}()},
		{name: "box clock not synced", key: box, signFor: home, event: &noClock, alert: Alert{Home: home},
			nowMs: fresh(connected), want: generic},
		{name: "box clock ahead of the phone", key: box, signFor: home, event: &ahead, alert: Alert{Home: home},
			nowMs: *ahead.WallMs - ClockLeadMs - 1, want: generic},
		{name: "a kind the phone does not know", key: box, signFor: home, event: &unknown, alert: Alert{Home: home},
			nowMs: fresh(unknown), want: generic},
		{name: "unreachable, from the service", alert: Alert{Home: home, Kind: protocol.PushBoxUnreachable},
			nowMs: wall, want: pushVectorOut{Verdict: AlertUnreachable, Home: home}},
		{name: "another kind from the service", alert: Alert{Home: home, Kind: protocol.PushChargingConnected},
			nowMs: wall, want: generic},
		{name: "a home the phone does not hold", key: box, signFor: home, event: &connected,
			alert: Alert{Home: "aaaaaaaaaaaaaaaaaaaaaaaaaa"}, nowMs: fresh(connected),
			want: pushVectorOut{Verdict: AlertGeneric, Home: "aaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{name: "box clock not synced, anchored by rule U", key: box, signFor: home, event: &noClock,
			alert: Alert{Home: home}, offset: ruleU, nowMs: fresh(connected), want: byRuleU(noClock, fresh(connected))},
		{name: "box clock ahead, anchored by rule U", key: box, signFor: home, event: &ahead,
			alert: Alert{Home: home}, offset: ruleU, nowMs: fresh(connected), want: byRuleU(ahead, fresh(connected))},
		{name: "rule U from another boot", key: box, signFor: home, event: &noClock,
			alert: Alert{Home: home}, offset: otherBoot, nowMs: fresh(connected), want: generic},
		{name: "rule U learned over a day ago", key: box, signFor: home, event: &noClock,
			alert: Alert{Home: home}, offset: dayOld, nowMs: fresh(connected), want: generic},
	}

	keys := map[string][]byte{home: boxPK, otherHome: otherPK}
	out := pushVectorFile{Keys: []pushVectorKey{
		{Home: home, PK: hex.EncodeToString(boxPK)},
		{Home: otherHome, PK: hex.EncodeToString(otherPK)},
	}}
	for i, s := range specs {
		a := s.alert
		var sig []byte
		if s.event != nil {
			body, err := Marshal(*s.event)
			if err != nil {
				t.Fatal(err)
			}
			sig = storedSig(i, s.name)
			if *update && Verify(&s.key.PublicKey, s.signFor, Envelope{T: MsgEvent, B: body, Sig: sig}) != nil {
				if sig, err = Sign(rand.Reader, s.key, s.signFor, MsgEvent, body); err != nil {
					t.Fatal(err)
				}
			}
			if err := Verify(&s.key.PublicKey, s.signFor, Envelope{T: MsgEvent, B: body, Sig: sig}); err != nil {
				t.Fatalf("%s: stored signature does not verify: %v", s.name, err)
			}
			env, err := Envelope{T: MsgEvent, B: body, Sig: sig}.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if s.mutate != nil {
				env = s.mutate(env)
			}
			a.Event = env
		}
		payload, err := AlertPayload(a)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > 4096 {
			t.Fatalf("%s: %d bytes is over APNs' 4 KB", s.name, len(payload))
		}

		offset := func(h string) *Offset {
			if h != home {
				return nil
			}
			return s.offset
		}
		view := ReadAlert(payload, func(h string) []byte { return keys[h] }, offset, s.nowMs)
		got := pushVectorOut{Verdict: view.Verdict, Home: view.Home, Kind: view.Kind, Args: view.Args,
			Src: view.Src, LP: view.LP, Seq: view.Seq, AtMs: view.AtMs}
		if view.Boot != nil {
			got.Boot = hex.EncodeToString(view.Boot)
		}
		if !equalOut(got, s.want) {
			t.Errorf("%s: got %+v, want %+v", s.name, got, s.want)
		}
		c := pushVectorCase{Name: s.name, Payload: string(payload), Sig: hex.EncodeToString(sig), NowMs: s.nowMs, Want: s.want}
		if s.offset != nil {
			c.Offset = &pushVectorOffset{Home: home, Boot: hex.EncodeToString(s.offset.Boot),
				OffsetMs: s.offset.OffsetMs, LearnedAtMs: s.offset.LearnedAtMs}
		}
		out.Cases = append(out.Cases, c)
	}

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(pushVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, _ := os.ReadFile(pushVectorsPath)
	if !bytes.Equal(got, want) {
		t.Fatalf("the bytes no longer match %s. If the change is intended, regenerate with -update "+
			"and pair it with srcfl/ftw-cloud and the app", pushVectorsPath)
	}
}

// fresh0 is a phone clock 1.2 s after the event's wallMs.
func fresh0(e Event) int64 { return *e.WallMs + 1_200 }

func equalOut(a, b pushVectorOut) bool {
	if len(a.Args) == 0 && len(b.Args) == 0 {
		a.Args, b.Args = nil, nil
	}
	ka, kb := make([]string, 0, len(a.Args)), make([]string, 0, len(b.Args))
	for k, v := range a.Args {
		ka = append(ka, k+"="+v)
	}
	for k, v := range b.Args {
		kb = append(kb, k+"="+v)
	}
	slices.Sort(ka)
	slices.Sort(kb)
	a.Args, b.Args = nil, nil
	return a.Verdict == b.Verdict && a.Home == b.Home && a.Kind == b.Kind && a.Src == b.Src && a.LP == b.LP &&
		a.Boot == b.Boot && a.Seq == b.Seq && a.AtMs == b.AtMs && slices.Equal(ka, kb)
}

// The service's alert has the same aps whatever it carries, so Apple learns
// a kind only from the signed event, never from priority or wording.
func TestEveryAlertHasTheSameAps(t *testing.T) {
	a, err := AlertPayload(Alert{Home: "h", Event: []byte{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := AlertPayload(Alert{Home: "h", Kind: protocol.PushBoxUnreachable})
	if err != nil {
		t.Fatal(err)
	}
	var pa, pb map[string]json.RawMessage
	if json.Unmarshal(a, &pa) != nil || json.Unmarshal(b, &pb) != nil {
		t.Fatal("not JSON")
	}
	if !bytes.Equal(pa["aps"], pb["aps"]) {
		t.Fatalf("aps differs: %s vs %s", pa["aps"], pb["aps"])
	}
	if !bytes.Contains(pa["aps"], []byte(`"mutable-content":1`)) {
		t.Fatalf("no mutable-content: %s", pa["aps"])
	}
}
