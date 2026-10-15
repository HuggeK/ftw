package cloudwire

// Fields is the allowlist of everything the box may send FTW Cloud: one table
// per class, and one per nested shape, as docs/protocol-cloud.md lists them.
// Header holds the four fields every box body carries.
//
// A field reaches a phone only under its class's scope (ClassScope); fields
// of a nested table belong to the class that carries it. Driver names, device
// capabilities and inventory have no row, so the box has no field to put
// them in. TestEveryFieldHasARow fails when a body type grows a field this
// table does not list, or the table lists one the type does not have.
var Fields = map[string][]string{
	"header": {"seq", "boot", "uptimeMs", "wallMs"},

	MsgHello: {"v", "pk", "nonce", "ekm", "boxNonce", "build", "clock", "caps", "rosterSeq", "grantId"},

	MsgLive:  {"fields", "fieldSources", "sources", "srcRev", "ages", "dispatchBlockedBy"},
	"Source": {"kind", "state", "lastOkMs", "staleAfterMs"},

	MsgLP: {"controlRev", "loadpoints", "feedback"},
	"Loadpoint": {
		"id", "src", "plugged", "socPermille", "socSource", "socAgeS",
		"targetPermille", "readyByMs", "recurring", "powerW", "sessionWh",
		"session", "manual", "commandedW", "commandedReason", "surplusOnly",
		"gridDeferred", "planPending", "boost", "nextStartMs", "nextEndMs", "nextWh",
	},
	"Manual":   {"state", "sinceAgeMs", "requestedW", "commandedW", "reason"},
	"Feedback": {"kind", "src", "status", "reason", "severity", "evidence", "fresh", "actualW", "ageMs"},

	MsgPrices: {"zone", "currency", "t0Ms", "slotMs", "spotMinor", "totalMinor"},

	MsgHB: {"clock", "srcRev", "liveSeq", "lpSeq", "ages"},

	MsgEvent: {"eventId", "ts", "kind", "severity", "args", "src", "lp"},

	MsgRoster: {"rosterSeq", "ttlS", "devices", "grant"},
	"Device":  {"id", "key", "role", "scopes", "epoch"},
	"Grant":   {"id", "scopes", "purposes", "controller"},

	MsgBye: {"why", "purge"},
}

// Bodies maps each class the box sends to its body type. Classes the box does
// not send yet (plan, health, tiles) have no entry; the service treats their
// bytes as opaque either way.
var Bodies = map[string]any{
	MsgHello:  Hello{},
	MsgLive:   Live{},
	MsgLP:     LP{},
	MsgPrices: Prices{},
	MsgHB:     HB{},
	MsgEvent:  Event{},
	MsgRoster: Roster{},
	MsgBye:    Bye{},
}
