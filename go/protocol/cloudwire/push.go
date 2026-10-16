package cloudwire

import (
	"bytes"
	"encoding/json"

	"github.com/srcfl/ftw/go/protocol"
)

// Events and push: what the box tells phones that are not looking, how a
// phone tells the service where Apple can reach it, and what the service
// hands Apple. docs/protocol-cloud.md, "Push", is the normative text.
//
// The box signs every event. The service wraps the envelope, unchanged, in an
// APNs alert; the phone's notification extension checks the signature with
// the box key it learned over Noise, writes its own text from the kind, and
// picks the buttons from the verified kind alone. The service picks nothing:
// not the words, not the buttons, not the priority. box.unreachable is the
// one alert the service writes itself, because a box that cannot reach the
// service cannot sign one; it carries no arguments and no buttons.

// Message types a phone sends the service over its app session.
const (
	MsgPushRegister   = "push.register"
	MsgPushUnregister = "push.unregister"
)

// APNs environments. A development build of the app gets sandbox tokens.
const (
	PushSandbox    = "sandbox"
	PushProduction = "production"
)

// PushKindAlert is the only kind of registration until widgets and Live
// Activities get their own pushes.
const PushKindAlert = "alert"

// APNsTokenMaxBytes bounds a device token. Apple's are 32 bytes today and
// documented as variable length.
const APNsTokenMaxBytes = 100

// Event severities, from the box's notification priority.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityAlarm   = "alarm"
)

// Event is one notification the box raised, for devices with
// ftw.events.read. Args are the catalogue's placeholders except names: an
// event about a device names it by Src, and the phone looks the name up in
// the table it holds from the box.
type Event struct {
	Header
	// EventID is unique per event.
	EventID string `cbor:"eventId"`
	// Ts is the box's wall time when the event happened, present only
	// while NTP has synced the box clock. It can be earlier than the
	// header's wallMs when the event waited for the uplink.
	Ts       *int64            `cbor:"ts,omitempty"`
	Kind     string            `cbor:"kind"`
	Severity string            `cbor:"severity"`
	Args     map[string]string `cbor:"args"`
	// Src is the opaque source id of the device the event concerns.
	Src string `cbor:"src,omitempty"`
	// LP is the opaque loadpoint id of the loadpoint it concerns.
	LP string `cbor:"lp,omitempty"`
	// Cars are opaque car ids, in the box's order: for car.which, the cars
	// the phone offers one button each.
	Cars []string `cbor:"cars,omitempty"`
}

// PushRegister tells the service a device's APNs token for one kind of push.
// The service binds it to the device's roster entry and forgets it when the
// roster drops the device, its epoch changes, or Apple says the token is
// gone.
type PushRegister struct {
	APNsToken   []byte `cbor:"apnsToken"`
	Environment string `cbor:"environment"`
	Kind        string `cbor:"kind"`
	// ActivityID names a Live Activity. Unused until its push exists.
	ActivityID string `cbor:"activityId,omitempty"`
	// App is the app the token belongs to, so the service picks the APNs
	// topic: PushAppIPhone, the default when absent, or PushAppWatch.
	App string `cbor:"app,omitempty"`
}

// Apps a push token can belong to (ADR 0009, decision 8). Apple sees for a
// watch what it sees for a phone, and no more.
const (
	PushAppIPhone = "iphone"
	PushAppWatch  = "watch"
)

// PushUnregister forgets one token.
type PushUnregister struct {
	APNsToken []byte `cbor:"apnsToken"`
}

// AlertMaxAgeMs is how old an event may be and still get its text and
// buttons on the phone. An older one, or one the phone cannot anchor in
// time, shows the generic line with no buttons.
const AlertMaxAgeMs = 10 * 60 * 1000

// UnreachableAfterMs is how long the service waits without the box's socket
// before it sends box.unreachable. A bye for an update holds it back until
// the box returns or UpdateGraceMs has passed.
const (
	UnreachableAfterMs = 600 * 1000
	UpdateGraceMs      = 60 * 60 * 1000
)

// Alert is the "ftw" member of an APNs alert payload. Event is the box's
// envelope exactly as it arrived; Kind is set only on the alert the service
// writes itself, box.unreachable, and a phone accepts no other kind there.
type Alert struct {
	V     int    `json:"v"`
	Home  string `json:"home"`
	Event []byte `json:"event,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

// AlertKey is the payload key Alert sits under, beside Apple's "aps".
const AlertKey = "ftw"

// Localisation keys of the line Apple shows if the extension does not run.
// The app's own strings hold the words, so the service writes none.
const (
	AlertTitleLocKey = "push.title"
	AlertBodyLocKey  = "push.generic"
)

// AlertPayload is the whole APNs payload for one alert: Apple's "aps" with
// the generic line and mutable-content, so the extension runs, and Alert
// under AlertKey. Every alert gets the same aps, whatever its kind.
func AlertPayload(a Alert) ([]byte, error) {
	type apsAlert struct {
		TitleLocKey string `json:"title-loc-key"`
		LocKey      string `json:"loc-key"`
	}
	type aps struct {
		Alert          apsAlert `json:"alert"`
		MutableContent int      `json:"mutable-content"`
		Sound          string   `json:"sound"`
	}
	a.V = Version
	return json.Marshal(struct {
		APS aps   `json:"aps"`
		FTW Alert `json:"ftw"`
	}{aps{apsAlert{AlertTitleLocKey, AlertBodyLocKey}, 1, "default"}, a})
}

// What a phone makes of an alert.
const (
	// AlertEvent: a box-signed event, anchored and fresh, of a kind the
	// catalogue has. The phone writes its text and offers its buttons.
	AlertEvent = "event"
	// AlertUnreachable: the service's box.unreachable. Its own text, no
	// buttons.
	AlertUnreachable = "unreachable"
	// AlertGeneric: anything else. The generic line, no buttons.
	AlertGeneric = "generic"
)

// AlertView is what an alert says once checked.
type AlertView struct {
	Verdict string
	Home    string
	// The rest is set only for AlertEvent.
	Kind string
	Args map[string]string
	Src  string
	LP   string
	Boot []byte
	Seq  uint64
	// AtMs is when the event happened by the phone's reading of the box
	// clock, less rule U's drift margin when rule U anchors it: never later
	// than it arrived.
	AtMs int64
}

// ClockLeadMs is how far ahead of the phone a box's wall clock may run
// before the phone stops trusting it (rule N).
const ClockLeadMs = 2000

// Offset is rule U's anchor for one home, as the phone keeps it for its
// notification extension: phone time minus box uptime, the smallest learned
// over Noise for Boot, and when it was last learned.
type Offset struct {
	Boot        []byte
	OffsetMs    int64
	LearnedAtMs int64
}

// ReadAlert is the reference for what a phone's notification extension does
// with an alert payload. boxKey returns the key the phone learned over Noise
// for a home, nil when it has none; offset returns rule U's anchor for a
// home, nil when it has none; nowMs is the phone's clock at arrival. Whether
// a (boot, seq) was seen before is the caller's state, not this function's.
func ReadAlert(payload []byte, boxKey func(home string) []byte, offset func(home string) *Offset, nowMs int64) AlertView {
	var p struct {
		FTW *Alert `json:"ftw"`
	}
	if json.Unmarshal(payload, &p) != nil || p.FTW == nil || p.FTW.V != Version {
		return AlertView{Verdict: AlertGeneric}
	}
	a := *p.FTW
	view := AlertView{Verdict: AlertGeneric, Home: a.Home}
	if len(a.Event) == 0 {
		if a.Kind == protocol.PushBoxUnreachable {
			view.Verdict = AlertUnreachable
		}
		return view
	}
	if a.Kind != "" {
		return view
	}
	env, err := DecodeEnvelope(a.Event)
	if err != nil || env.T != MsgEvent {
		return view
	}
	pk := boxKey(a.Home)
	if pk == nil {
		return view
	}
	pub, err := ParsePublicKey(pk)
	if err != nil || Verify(pub, a.Home, env) != nil {
		return view
	}
	var ev Event
	if Unmarshal(env.B, &ev) != nil {
		return view
	}
	at, ok := alertSendTime(ev, offset(a.Home), nowMs)
	if !ok {
		return view
	}
	if ev.Ts != nil {
		at = min(at, *ev.Ts)
	}
	_, known := protocol.PushSentences[ev.Kind]
	if !known || ev.Kind == protocol.PushBoxUnreachable || nowMs-at > AlertMaxAgeMs {
		return view
	}
	return AlertView{Verdict: AlertEvent, Home: a.Home, Kind: ev.Kind, Args: ev.Args, Src: ev.Src, LP: ev.LP,
		Boot: ev.Boot, Seq: ev.Seq, AtMs: at}
}

// alertSendTime anchors an event as View anchors a message: the earlier of
// rule N and rule U, and when rule U gives it, earlier by the drift margin so
// the margin counts against the age. A box clock more than ClockLeadMs ahead
// gives no rule N. ok is false when neither rule applies.
func alertSendTime(ev Event, off *Offset, nowMs int64) (at int64, ok bool) {
	if ev.WallMs != nil && *ev.WallMs <= nowMs+ClockLeadMs {
		at, ok = min(*ev.WallMs, nowMs), true
	}
	if off == nil || !bytes.Equal(off.Boot, ev.Boot) || nowMs-off.LearnedAtMs > RuleULifetimeMs {
		return at, ok
	}
	if u := min(off.OffsetMs+ev.UptimeMs, nowMs); !ok || u <= at {
		at, ok = u-max(0, nowMs-off.LearnedAtMs)/ruleUDriftDivisor, true
	}
	return at, ok
}
