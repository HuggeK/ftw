package cloudwire

import "github.com/srcfl/ftw/go/protocol"

// Message types. The service sends exactly the first five and tun; anything
// else from it closes the uplink.
const (
	MsgChallenge = "challenge"
	MsgHelloOK   = "hello_ok"
	MsgWatch     = "watch"
	MsgResync    = "resync"
	MsgPurged    = "purged"
)

// Message types the box sends.
const (
	MsgHello  = "hello"
	MsgLive   = "live"
	MsgLP     = "lp"
	MsgPrices = "prices"
	MsgPlan   = "plan"
	MsgHealth = "health"
	MsgEvent  = "event"
	MsgTiles  = "tiles"
	MsgHB     = "hb"
	MsgRoster = "roster"
	MsgBye    = "bye"
	// MsgNotice tells the owners' phones that something changed who may
	// reach the box (docs/agents.md, Push kinds). It carries the kind and
	// nothing else, and the box sends it whenever it is linked, copy or not.
	MsgNotice = "notice"
)

// Downlink is the closed set of types the service may send the box. None of
// the first five asks the box to act. tun carries ciphertext the box decrypts
// itself and judges as it judges the same session over the relay.
var Downlink = []string{MsgChallenge, MsgHelloOK, MsgWatch, MsgResync, MsgPurged, MsgTun}

// ClassScope is the scope a device needs to read each class the service
// forwards. hb maps to "" because every device in the roster gets it: it
// carries no readings, only how old they are.
var ClassScope = map[string]string{
	MsgLive:   protocol.ScopeLiveRead,
	MsgLP:     protocol.ScopeLiveRead,
	MsgPrices: protocol.ScopeLiveRead,
	MsgPlan:   protocol.ScopePlanRead,
	MsgHealth: protocol.ScopeHealthRead,
	MsgEvent:  protocol.ScopeEventsRead,
	MsgTiles:  protocol.ScopeHistoryRead,
	MsgHB:     "",
}

// ServiceOnly are box messages the service acts on and never forwards.
var ServiceOnly = []string{MsgHello, MsgRoster, MsgBye, MsgNotice}

// Clock values for hello.clock and hb.clock.
const (
	ClockNTP  = "ntp"
	ClockNone = "none"
)

// Bye reasons.
const (
	ByeRevoked  = "revoked"
	ByeShutdown = "shutdown"
	ByeUpdate   = "update"
)

// What a purge receipt or a resync names. PurgedCopy confirms that the
// readable copy of a grant is gone while the home stays, for its tunnel.
const (
	PurgedHome  = "home"
	PurgedCopy  = "copy"
	PurgedStats = "stats"

	ResyncFull   = "full"
	ResyncTiles  = "tiles"
	ResyncRoster = "roster"
)

// ControllerSourceful is the data controller unless a partner flow names
// another. No such flow exists yet.
const ControllerSourceful = "sourceful"

// --------------------------------------------------------------------------
// Service to box
// --------------------------------------------------------------------------

// Challenge is the service's first message, unsigned.
type Challenge struct {
	V     int    `cbor:"v"`
	Nonce []byte `cbor:"nonce"`
}

// HelloOK answers hello. The service signs it, and the box sends nothing but
// hello before it has checked it.
type HelloOK struct {
	BoxNonce []byte `cbor:"boxNonce"`
	// EKM is the service's TLS exporter value for this session. A proxy that
	// ends TLS holds two sessions with two values, so the check fails there.
	EKM []byte `cbor:"ekm"`
	// Have maps tile id to etag for the tiles the service holds.
	Have      map[string]string `cbor:"have"`
	RosterSeq uint64            `cbor:"rosterSeq"`
}

// Watch raises the period to one second for 60 seconds.
type Watch struct {
	Hz int `cbor:"hz"`
}

// Resync asks for full state again. The box honours one per five minutes.
type Resync struct {
	What  []string `cbor:"what"`
	Tiles []string `cbor:"tiles,omitempty"`
}

// Purged is the service's signed receipt that a deletion is done.
type Purged struct {
	AtMs    int64  `cbor:"atMs"`
	What    string `cbor:"what"`
	GrantID []byte `cbor:"grantId"`
	StatsID []byte `cbor:"statsId,omitempty"`
}

// --------------------------------------------------------------------------
// Box to service
// --------------------------------------------------------------------------

// Header is in every body the box sends.
type Header struct {
	// Seq starts at 1 and rises by 1 per message, per boot, across
	// reconnects.
	Seq uint64 `cbor:"seq"`
	// Boot is drawn at random at each Core start.
	Boot []byte `cbor:"boot"`
	// UptimeMs is the box's monotonic time since start, at send.
	UptimeMs int64 `cbor:"uptimeMs"`
	// WallMs is the box's wall time at send, present only while NTP has
	// synced the box clock.
	WallMs *int64 `cbor:"wallMs,omitempty"`
}

// Hello proves the box holds its key and binds this TLS session to it.
type Hello struct {
	Header
	V int `cbor:"v"`
	// PK is the box's public key, 65 bytes. The home id is a hash of it.
	PK       []byte `cbor:"pk"`
	Nonce    []byte `cbor:"nonce"`
	EKM      []byte `cbor:"ekm"`
	BoxNonce []byte `cbor:"boxNonce"`
	Build    string `cbor:"build"`
	Clock    string `cbor:"clock"`
	// Caps lists the classes this box sends.
	Caps      []string `cbor:"caps"`
	RosterSeq uint64   `cbor:"rosterSeq"`
	GrantID   []byte   `cbor:"grantId"`
}

// Live is the site's flows and the freshness of every source behind them.
type Live struct {
	Header
	// Fields uses the app protocol's frozen ids 1–6 and 10, and its extra
	// fields 11–14 when the site meter gives phases, keyed and encoded as its
	// delta does.
	Fields map[string]int64 `cbor:"fields"`
	// FieldSources and Sources travel only when the source table changed and
	// in every full message.
	FieldSources map[string][]string `cbor:"fieldSources,omitempty"`
	Sources      map[string]Source   `cbor:"sources,omitempty"`
	// SrcRev rises by 1, per boot, each time the source table changes.
	SrcRev uint64 `cbor:"srcRev"`
	// Ages is, for each live or lagging source, the age of its newest
	// reading at send, in ms of box uptime.
	Ages              map[string]int64 `cbor:"ages"`
	DispatchBlockedBy []string         `cbor:"dispatchBlockedBy"`
}

// Source is one source's freshness as the box grades it.
type Source struct {
	Kind         string               `cbor:"kind"`
	State        protocol.SourceState `cbor:"state"`
	LastOkMs     int64                `cbor:"lastOkMs"`
	StaleAfterMs int64                `cbor:"staleAfterMs"`
}

// LP is loadpoint and control feedback state.
type LP struct {
	Header
	ControlRev uint64      `cbor:"controlRev"`
	Loadpoints []Loadpoint `cbor:"loadpoints"`
	Feedback   []Feedback  `cbor:"feedback"`
}

// Loadpoint is one charger as the household sees it. Capabilities such as
// maximum power or phases are not sent.
type Loadpoint struct {
	ID              string  `cbor:"id"`
	Src             string  `cbor:"src"`
	Plugged         bool    `cbor:"plugged"`
	SocPermille     *int64  `cbor:"socPermille"`
	SocSource       string  `cbor:"socSource"`
	SocAgeS         *int64  `cbor:"socAgeS"`
	TargetPermille  *int64  `cbor:"targetPermille"`
	ReadyByMs       *int64  `cbor:"readyByMs"`
	Recurring       bool    `cbor:"recurring"`
	PowerW          int64   `cbor:"powerW"`
	SessionWh       int64   `cbor:"sessionWh"`
	Session         string  `cbor:"session"`
	Manual          *Manual `cbor:"manual"`
	CommandedW      *int64  `cbor:"commandedW"`
	CommandedReason string  `cbor:"commandedReason"`
	SurplusOnly     bool    `cbor:"surplusOnly"`
	GridDeferred    bool    `cbor:"gridDeferred"`
	PlanPending     bool    `cbor:"planPending"`
	Boost           bool    `cbor:"boost"`
	NextStartMs     *int64  `cbor:"nextStartMs"`
	NextEndMs       *int64  `cbor:"nextEndMs"`
	NextWh          *int64  `cbor:"nextWh"`
}

// Manual is a manual charging hold.
type Manual struct {
	State      string `cbor:"state"`
	SinceAgeMs int64  `cbor:"sinceAgeMs"`
	RequestedW *int64 `cbor:"requestedW"`
	CommandedW *int64 `cbor:"commandedW"`
	Reason     string `cbor:"reason"`
}

// Feedback is Core's verdict on whether one device function follows.
type Feedback struct {
	Kind     string `cbor:"kind"`
	Src      string `cbor:"src"`
	Status   string `cbor:"status"`
	Reason   string `cbor:"reason"`
	Severity string `cbor:"severity"`
	Evidence string `cbor:"evidence"`
	Fresh    bool   `cbor:"fresh"`
	ActualW  *int64 `cbor:"actualW"`
	AgeMs    *int64 `cbor:"ageMs"`
}

// Prices is the price window, in columns.
type Prices struct {
	Header
	Zone     string `cbor:"zone"`
	Currency string `cbor:"currency"`
	T0Ms     int64  `cbor:"t0Ms"`
	SlotMs   int64  `cbor:"slotMs"`
	// SpotMinor and TotalMinor are minor units per kWh, null where a slot
	// has no price. TotalMinor includes tariff and VAT.
	SpotMinor  []*int64 `cbor:"spotMinor"`
	TotalMinor []*int64 `cbor:"totalMinor"`
}

// HB says the box is there and how old its readings are. It never vouches
// for a live or lp the box has not sent: it names the newest of each.
type HB struct {
	Header
	Clock   string           `cbor:"clock"`
	SrcRev  uint64           `cbor:"srcRev"`
	LiveSeq uint64           `cbor:"liveSeq"`
	LpSeq   uint64           `cbor:"lpSeq"`
	Ages    map[string]int64 `cbor:"ages"`
}

// Roster is every device paired with the box and what the owner granted.
// The service learns devices from nothing else.
type Roster struct {
	Header
	RosterSeq uint64   `cbor:"rosterSeq"`
	TTLS      int64    `cbor:"ttlS"`
	Devices   []Device `cbor:"devices"`
	Grant     Grant    `cbor:"grant"`
	// Pending lists the pairing codes the box shows now. Each admits one
	// first session through the service, for a phone the roster does not
	// list yet.
	Pending []Pending `cbor:"pending"`
	// Noise is the box's X25519 Noise static key, the one in its pairing
	// QR code. FTW Cloud's keys part opens an agent's session to it, and
	// trusts it only as signed here, by the home's key.
	Noise []byte `cbor:"noise"`
}

// Device is one paired phone.
type Device struct {
	ID string `cbor:"id"`
	// Key is the device's X25519 Noise static key.
	Key  []byte `cbor:"key"`
	Role string `cbor:"role"`
	// Scopes are what the box derives from the role. The service holds no
	// role table.
	Scopes []string `cbor:"scopes"`
	Epoch  uint64   `cbor:"epoch"`
	// Tun says the device may open a session to the box through the
	// service. It needs no grant: the readable copy is a purpose on top.
	Tun bool `cbor:"tun"`
	// Agent is set on an agent row: an assistant, Sourceful support or the
	// release check (docs/agents.md). Absent on a phone or a watch.
	Agent *Agent `cbor:"agent,omitempty"`
}

// Agent is an agent row's grant, as the box enforces it. The service must
// stop admitting the row at ExpiresAtMs by its own clock, never admit it
// through the relay, and may tighten the caps but never loosen them.
type Agent struct {
	// Scopes and Ops are the grant at the approving owner's scopes now.
	Scopes      []string           `cbor:"scopes"`
	Ops         []string           `cbor:"ops"`
	Caps        protocol.Caps      `cbor:"caps"`
	ExpiresAtMs int64              `cbor:"expiresAtMs"`
	Principal   protocol.Principal `cbor:"principal"`
}

// Pending is one pairing code on the box's screen, by its PairingHash.
type Pending struct {
	Hash []byte `cbor:"hash"`
	// TTLS is how long the code has left, counted from when the service
	// receives the roster.
	TTLS int64 `cbor:"ttlS"`
}

// Grant is what the owner turned on. With the readable copy off it has no
// id, scopes or purposes. While a revoke waits for its receipt it keeps the
// id and has neither scopes nor purposes.
type Grant struct {
	ID         []byte   `cbor:"id"`
	Scopes     []string `cbor:"scopes"`
	Purposes   []string `cbor:"purposes"`
	Controller string   `cbor:"controller"`
}

// Notice is a security notice for the owners: a push kind from the
// catalogue, such as agent.added, and nothing that says more. The service
// sends each owner device registered for alerts a push with no data.
type Notice struct {
	Header
	Kind string `cbor:"kind"`
}

// NoticeKinds are the kinds a notice may carry.
var NoticeKinds = []string{protocol.PushAgentAdded, protocol.PushAgentRequest, protocol.PushAgentExpiring,
	protocol.PushApprovalKeySet, protocol.PushOwnerAdded}

// Bye ends the uplink. Purge asks the service to delete the home.
type Bye struct {
	Header
	Why   string `cbor:"why"`
	Purge bool   `cbor:"purge"`
}
