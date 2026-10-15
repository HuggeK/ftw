// Package cloudlink streams a signed, readable copy of the box's state to FTW
// Cloud, when an owner has turned it on. docs/protocol-cloud.md is the wire
// format and docs/adr/0008-ftw-cloud.md the decision.
//
// FTW Cloud reads and never controls. This package holds that line in its
// build: it reaches the box only through the read-only ports below and may
// import nothing from the box (imports_test.go and CI check it with go list
// -deps). What the service may send back is a closed set of five messages,
// and none of them asks the box to do anything but send sooner, send again or
// forget a key.
//
// The link is off until an owner grants it. A missing grant, or one whose key
// fingerprint does not match cloud.key, reads as off.
package cloudlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Compile-time settings. Production builds use the defaults; a local test
// build may set them with -ldflags -X. A box that can be pointed at another
// service at runtime is a box a support call can point at an attacker's.
var (
	// Endpoint is the service's uplink.
	Endpoint = "wss://cloud.ftw.energy/v1/box"
	// ServiceKeys are the service's P-256 public keys, base64url of the
	// 65-byte point, comma separated. Up to two, so the key can rotate
	// across a release (docs/protocol-cloud.md, "Keys and ids"). Empty
	// means no service exists for this build: the box refuses a grant and
	// never dials.
	//
	// The one key is cloud.ftw.energy's, in Secrets Manager as
	// ftw-cloud/service-key since srcfl/ftw-cloud#3. Only its public half is
	// here; a box checks every hello_ok and downlink message against it.
	ServiceKeys = "BG6EvBxFzSHPVsQOES6PYaKFo5vHQmte9lJ6D5OBIT9JgcBrJAOxAucdqKXxS76pToQ7orI1Ivsno0OtxbMuliQ"
	// DevRootCA is a PEM file of extra roots, for a local service with its
	// own certificate. Empty in every release build.
	DevRootCA = ""
)

// ErrNoService is a grant on a build that names no service key.
var ErrNoService = errors.New("cloudlink: this build has no FTW Cloud service")

// ErrPurgePending is a grant asked for while the last one is still being
// deleted at the service.
var ErrPurgePending = errors.New("cloudlink: FTW Cloud is still deleting the previous copy")

// ErrNotGranted is statistics asked for while FTW Cloud is off.
var ErrNotGranted = errors.New("cloudlink: turn FTW Cloud on first")

// statsIDBytes is the length of a statsId, which the protocol fixes at 16.
const statsIDBytes = 16

// Site is the box's state, read. Every method answers from memory and changes
// nothing. Names in the answers are the box's own driver and loadpoint names;
// the link replaces each with an opaque id before anything leaves the house.
type Site interface {
	// Live is the frozen fields and the source table.
	Live() Live
	// Loadpoints is loadpoint state and control feedback. ok is false on a
	// box without loadpoints, which then sends no lp.
	Loadpoints() (lp cloudwire.LP, ok bool)
	// Prices is the price window from now. ok is false when the box has no
	// prices.
	Prices() (p cloudwire.Prices, ok bool)
}

// Live is the live class before ids are cut.
type Live struct {
	// Fields uses the app protocol's frozen ids as text keys.
	Fields map[string]int64
	// FieldSources names, per field, the drivers its freshness follows.
	FieldSources map[string][]string
	// Sources is keyed by driver name.
	Sources map[string]Source
	// DispatchBlockedBy names drivers whose staleness stops dispatch.
	DispatchBlockedBy []string
}

// Source is one driver's freshness.
type Source struct {
	Kind  string
	State protocol.SourceState
	// LastOkUptimeMs is box uptime at the last good reading, negative when
	// there was none.
	LastOkUptimeMs int64
	StaleAfterMs   int64
}

// RosterDevice is one paired phone.
type RosterDevice struct {
	ID    string
	Key   []byte
	Role  string
	Epoch uint64
	// Scopes are what the role carries, expanded by the box.
	Scopes []string
}

// Roster lists the paired phones.
type Roster interface {
	Devices() []RosterDevice
}

// Options wires a link. Site, Roster, KV and KeyPath are required.
type Options struct {
	Site    Site
	Roster  Roster
	KV      KV
	KeyPath string
	Build   string
	// Started anchors uptime, the clock every age is measured against.
	Started time.Time
	// ClockSynced reports whether NTP has set the wall clock. Nil reads as
	// not synced, and wallMs is then left out.
	ClockSynced func() bool
	// Notify tells owners the grant changed, with protocol.PushCloudGranted
	// or PushCloudRevoked. Nil sends nothing.
	Notify func(kind string)
	Logger *slog.Logger

	// Seams for tests. Production leaves them zero.
	Endpoint    string
	ServiceKeys []*ecdsa.PublicKey
	RootCAs     *x509.CertPool
	Tick        time.Duration
}

// Link is the box's one uplink to FTW Cloud.
type Link struct {
	opts Options
	log  *slog.Logger
	boot []byte

	// wake is signalled when the grant or the roster changes, so the
	// sender acts within one tick.
	wake chan struct{}

	mu        sync.Mutex
	seq       uint64
	key       *ecdsa.PrivateKey
	pk        []byte
	home      string
	grant     *grantRecord
	notice    string
	connected bool
	lastSend  time.Time
	budget    budget
	sent      map[string]sentFrame
	srcRev    uint64
	table     map[string]cloudwire.Source
	tableKey  string
	liveSeq   uint64
	lpSeq     uint64
	resyncAt  time.Time
	attempt   int
	purgedAt  int64
	rosterDue bool

	// events wait for the writer; eventsDropped counts those that never
	// left. closing is set by Bye, after which the link does not dial
	// again.
	events        []EventIn
	eventsDropped int64
	closing       bool
	byeWhy        string
	byeDone       chan struct{}
}

// New loads the grant and the key and returns a link that runs once Run is
// called.
func New(opts Options) (*Link, error) {
	if opts.Site == nil || opts.Roster == nil || opts.KV == nil || opts.KeyPath == "" {
		return nil, errors.New("cloudlink: site, roster, state store and key path are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Endpoint == "" {
		opts.Endpoint = Endpoint
	}
	if opts.ServiceKeys == nil {
		keys, err := parseServiceKeys(ServiceKeys)
		if err != nil {
			return nil, err
		}
		opts.ServiceKeys = keys
	}
	if opts.RootCAs == nil && DevRootCA != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pemBytes, err := os.ReadFile(DevRootCA)
		if err != nil {
			return nil, fmt.Errorf("cloudlink: reading dev root CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("cloudlink: %s holds no certificate Go can use", DevRootCA)
		}
		opts.RootCAs = pool
	}
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	if opts.Tick <= 0 {
		opts.Tick = 250 * time.Millisecond
	}
	boot, err := randomBytes(cloudwire.BootBytes)
	if err != nil {
		return nil, err
	}
	l := &Link{
		opts: opts, log: opts.Logger.With("component", "cloudlink"), boot: boot,
		wake: make(chan struct{}, 1), sent: map[string]sentFrame{},
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func parseServiceKeys(list string) ([]*ecdsa.PublicKey, error) {
	var keys []*ecdsa.PublicKey
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("cloudlink: service key: %w", err)
		}
		pub, err := cloudwire.ParsePublicKey(raw)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pub)
	}
	if len(keys) > 2 {
		return nil, errors.New("cloudlink: at most two service keys")
	}
	return keys, nil
}

// load reads the grant and the key. A grant without its key, or with another
// key, reads as off and leaves a notice.
func (l *Link) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.grant, l.key, l.pk, l.home, l.notice = nil, nil, nil, "", ""
	if r, err := loadJSON[receipt](l.opts.KV, purgedKey); err == nil && r != nil {
		l.purgedAt = r.AtMs
	}
	g, err := loadJSON[grantRecord](l.opts.KV, grantKey)
	if err != nil {
		l.notice = "grant_unreadable"
		l.log.Warn("FTW Cloud grant unreadable; treating it as off", "err", err)
		return nil
	}
	if g == nil {
		return nil
	}
	key, err := loadKey(l.opts.KeyPath)
	if err != nil {
		l.notice = "key_missing"
		l.log.Warn("FTW Cloud is off: the grant has no key on this box", "err", err)
		return nil
	}
	fp, pk, err := fingerprint(key)
	if err != nil || fp != g.KeyFp {
		l.notice = "key_mismatch"
		l.log.Warn("FTW Cloud is off: cloud.key is not the key the grant was made with")
		return nil
	}
	l.grant, l.key, l.pk, l.home = g, key, pk, cloudwire.HomeID(pk)
	return nil
}

// signal wakes the sender. It never blocks.
func (l *Link) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// RosterChanged is called by enrolment after a pair, role change or revoke.
func (l *Link) RosterChanged() {
	l.mu.Lock()
	l.rosterDue = true
	l.mu.Unlock()
	l.signal()
}

// GrantRequest is an owner's request to turn FTW Cloud on.
type GrantRequest struct {
	Scopes []string `json:"scopes"`
	// Policy names the consent text the owner read.
	Policy string `json:"policy"`
	// By is the device or door that asked, for the audit line.
	By string `json:"-"`
}

// Grant turns FTW Cloud on, or changes the scopes of a grant already on.
func (l *Link) Grant(req GrantRequest) error {
	if len(l.opts.ServiceKeys) == 0 {
		return ErrNoService
	}
	scopes, err := checkScopes(req.Scopes)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Policy) == "" {
		return fmt.Errorf("%w: name the policy the owner accepted", ErrGrantRequest)
	}

	l.mu.Lock()
	switch {
	case l.grant != nil && l.grant.Revoked:
		l.mu.Unlock()
		return ErrPurgePending
	case l.grant != nil:
		g := *l.grant
		g.Scopes, g.Policy, g.By = scopes, req.Policy, req.By
		if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
			l.mu.Unlock()
			return err
		}
		l.grant = &g
		l.rosterDue = true
		l.mu.Unlock()
		l.audit("changed", &g)
		l.signal()
		return nil
	}
	l.mu.Unlock()

	key, err := createKey(l.opts.KeyPath)
	if err != nil {
		return err
	}
	fp, pk, err := fingerprint(key)
	if err != nil {
		return err
	}
	id, err := randomBytes(cloudwire.GrantIDBytes)
	if err != nil {
		return err
	}
	gk, err := randomBytes(cloudwire.GrantKeyBytes)
	if err != nil {
		return err
	}
	g := grantRecord{
		ID: id, Scopes: scopes, Purposes: []string{protocol.CloudPurposeServe},
		KeyFp: fp, GrantKey: gk, By: req.By, Policy: req.Policy,
		Controller: cloudwire.ControllerSourceful,
	}
	if l.clockSynced() {
		g.AtMs = time.Now().UnixMilli()
	}
	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		_ = os.Remove(l.opts.KeyPath)
		return err
	}

	l.mu.Lock()
	l.grant, l.key, l.pk, l.home, l.notice = &g, key, pk, cloudwire.HomeID(pk), ""
	l.table, l.tableKey, l.sent, l.rosterDue = nil, "", map[string]sentFrame{}, true
	l.mu.Unlock()

	l.audit("granted", &g)
	l.notify(protocol.PushCloudGranted)
	l.signal()
	return nil
}

// Revoke turns FTW Cloud off. The box stops sending readings at once, asks
// the service to delete the home, and keeps the key until the service's
// signed receipt arrives.
func (l *Link) Revoke(by string) error {
	l.mu.Lock()
	if l.grant == nil || l.grant.Revoked {
		l.mu.Unlock()
		return nil
	}
	g := *l.grant
	g.Revoked, g.By = true, by
	if l.clockSynced() {
		g.RevokedAtMs = time.Now().UnixMilli()
	}
	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		l.mu.Unlock()
		return err
	}
	l.grant = &g
	l.mu.Unlock()

	l.audit("revoked", &g)
	l.notify(protocol.PushCloudRevoked)
	l.signal()
	return nil
}

// StatsRequest is an owner's request to contribute to statistics.
type StatsRequest struct {
	// Policy names the consent text the owner read.
	Policy string `json:"policy"`
	// By is the device or door that asked, for the audit line.
	By string `json:"-"`
}

// Stats adds the stats purpose to a grant already on. The first time, it
// draws the statsId that names this box's rows at the service; later calls
// keep it. It sends no statistics itself.
func (l *Link) Stats(req StatsRequest) error {
	if len(l.opts.ServiceKeys) == 0 {
		return ErrNoService
	}
	if strings.TrimSpace(req.Policy) == "" {
		return fmt.Errorf("%w: name the policy the owner accepted", ErrGrantRequest)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.grant == nil:
		return ErrNotGranted
	case l.grant.Revoked:
		return ErrPurgePending
	}
	g := *l.grant
	if len(g.StatsID) == 0 {
		id, err := randomBytes(statsIDBytes)
		if err != nil {
			return err
		}
		g.StatsID = id
	}
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, true)
	g.StatsPolicy, g.By = req.Policy, req.By
	return l.storeLocked(g, "statistics on")
}

// StopStats removes the stats purpose and forgets the statsId. Turning it on
// again draws a new one.
func (l *Link) StopStats(by string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.grant == nil || l.grant.Revoked ||
		(len(l.grant.StatsID) == 0 && !slices.Contains(l.grant.Purposes, protocol.CloudPurposeStats)) {
		return nil
	}
	g := *l.grant
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, false)
	g.StatsID, g.StatsPolicy, g.By = nil, "", by
	return l.storeLocked(g, "statistics off")
}

// storeLocked saves a changed grant, marks the roster due and writes the
// audit line. The caller holds l.mu.
func (l *Link) storeLocked(g grantRecord, event string) error {
	if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
		return err
	}
	l.grant = &g
	l.rosterDue = true
	l.audit(event, &g)
	l.signal()
	return nil
}

// withPurpose returns purposes with p present or absent, in registry order.
// It never changes the slice it was given.
func withPurpose(purposes []string, p string, on bool) []string {
	out := []string{}
	for _, q := range protocol.CloudPurposes {
		if (q == p && on) || (q != p && slices.Contains(purposes, q)) {
			out = append(out, q)
		}
	}
	return out
}

// finishPurge deletes the key and the grant once the service has confirmed
// the deletion.
func (l *Link) finishPurge(atMs int64) {
	l.mu.Lock()
	g := l.grant
	if g == nil || !g.Revoked {
		l.mu.Unlock()
		return
	}
	if err := saveJSON(l.opts.KV, purgedKey, receipt{AtMs: atMs, GrantID: g.ID}); err != nil {
		l.log.Warn("could not store the FTW Cloud purge receipt", "err", err)
	}
	if err := saveJSON(l.opts.KV, grantKey, nil); err != nil {
		l.log.Warn("could not clear the FTW Cloud grant", "err", err)
	}
	if err := os.Remove(l.opts.KeyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.log.Warn("could not delete cloud.key", "err", err)
	}
	l.grant, l.key, l.pk, l.home, l.purgedAt = nil, nil, nil, "", atMs
	l.table, l.tableKey, l.sent = nil, "", map[string]sentFrame{}
	l.mu.Unlock()
	l.audit("purged", g)
}

// audit writes the line that records a grant change on the box.
func (l *Link) audit(event string, g *grantRecord) {
	l.log.Info("audit: FTW Cloud grant "+event,
		"grant", hex.EncodeToString(g.ID), "by", g.By, "scopes", g.Scopes,
		"purposes", g.Purposes, "policy", g.Policy, "stats_policy", g.StatsPolicy)
}

func (l *Link) notify(kind string) {
	if l.opts.Notify != nil {
		go l.opts.Notify(kind)
	}
}

func (l *Link) clockSynced() bool {
	return l.opts.ClockSynced != nil && l.opts.ClockSynced()
}

// Status is what GET /api/cloud shows.
type Status struct {
	// State is off, on or revoking.
	State string `json:"state"`
	// Notice says why a grant on disk reads as off: key_missing,
	// key_mismatch or grant_unreadable.
	Notice      string   `json:"notice,omitempty"`
	Available   bool     `json:"available"`
	Home        string   `json:"home,omitempty"`
	Scopes      []string `json:"scopes"`
	Purposes    []string `json:"purposes"`
	GrantedAtMs int64    `json:"grantedAtMs,omitempty"`
	GrantedBy   string   `json:"grantedBy,omitempty"`
	Policy      string   `json:"policy,omitempty"`
	Connected   bool     `json:"connected"`
	// LastSendAgeMs is how long ago the box last sent anything, -1 never.
	LastSendAgeMs int64 `json:"lastSendAgeMs"`
	// BytesLast24h counts what the box sent in the last 24 hours of uptime.
	BytesLast24h  int64 `json:"bytesLast24h"`
	BudgetBytes   int64 `json:"budgetBytes"`
	BudgetReached bool  `json:"budgetReached"`
	// PurgedAtMs is the time on the service's receipt for the last deletion.
	PurgedAtMs int64 `json:"purgedAtMs,omitempty"`
	// EventsDropped counts notifications that never reached the service:
	// the queue was full, or the uplink was down for longer than a phone
	// would still act on them.
	EventsDropped int64 `json:"eventsDropped"`
}

// Status reports the grant and the link.
func (l *Link) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Status{
		State: "off", Notice: l.notice, Available: len(l.opts.ServiceKeys) > 0,
		Scopes: []string{}, Purposes: []string{}, Connected: l.connected,
		LastSendAgeMs: -1, PurgedAtMs: l.purgedAt, EventsDropped: l.eventsDropped,
		BytesLast24h: l.budget.total(l.uptimeMs()), BudgetBytes: budgetBytes,
	}
	s.BudgetReached = s.BytesLast24h >= budgetBytes
	if !l.lastSend.IsZero() {
		s.LastSendAgeMs = time.Since(l.lastSend).Milliseconds()
	}
	if g := l.grant; g != nil {
		s.State, s.Home, s.Scopes, s.Purposes = "on", l.home, g.Scopes, g.Purposes
		s.GrantedAtMs, s.GrantedBy, s.Policy = g.AtMs, g.By, g.Policy
		if g.Revoked {
			s.State = "revoking"
		}
	}
	return s
}

// Available reports whether this build names a service, and so can run the
// uplink at all. Release builds without service keys cannot.
func (l *Link) Available() bool { return len(l.opts.ServiceKeys) > 0 }

// SessionInfo is what a phone needs, over its Noise session with the box, to
// read this home from FTW Cloud: the key that signs it, the home id, the boot
// that anchors its times and names for the opaque ids. It is nil unless the
// grant is on.
//
// The tables name every driver and loadpoint the box knows now. One that
// appears later has no name on a phone until its next session.
func (l *Link) SessionInfo() *protocol.CloudSession {
	snap := l.current()
	if snap.g == nil || snap.g.Revoked || len(snap.pk) != cloudwire.PublicKeyBytes {
		return nil
	}
	gk := snap.g.GrantKey
	s := &protocol.CloudSession{
		PK: bytes.Clone(snap.pk), Home: snap.home, Boot: bytes.Clone(l.boot),
		Sources: map[string]string{}, Loadpoints: map[string]string{},
	}
	source := func(name string) {
		if name != "" {
			s.Sources[cloudwire.SourceID(gk, name)] = name
		}
	}
	live := l.opts.Site.Live()
	for name := range live.Sources {
		source(name)
	}
	for _, names := range live.FieldSources {
		for _, name := range names {
			source(name)
		}
	}
	for _, name := range live.DispatchBlockedBy {
		source(name)
	}
	if lp, ok := l.opts.Site.Loadpoints(); ok {
		for _, p := range lp.Loadpoints {
			s.Loadpoints[cloudwire.LoadpointID(gk, p.ID)] = p.ID
			source(p.Src)
		}
		for _, f := range lp.Feedback {
			source(f.Src)
		}
	}
	return s
}

func (l *Link) uptimeMs() int64 { return time.Since(l.opts.Started).Milliseconds() }

// header stamps the next box body. Seq rises per message, per boot.
func (l *Link) header() cloudwire.Header {
	l.seq++
	h := cloudwire.Header{Seq: l.seq, Boot: l.boot, UptimeMs: l.uptimeMs()}
	if l.clockSynced() {
		w := time.Now().UnixMilli()
		h.WallMs = &w
	}
	return h
}

// grantSnapshot is the grant and key as one consistent read.
type grantSnapshot struct {
	g    *grantRecord
	key  *ecdsa.PrivateKey
	pk   []byte
	home string
}

func (l *Link) current() grantSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return grantSnapshot{g: l.grant, key: l.key, pk: l.pk, home: l.home}
}

// Run keeps the uplink up while a grant is on, until ctx ends.
func (l *Link) Run(ctx context.Context) error {
	for {
		if l.isClosing() {
			return nil
		}
		snap := l.current()
		if snap.g == nil || len(l.opts.ServiceKeys) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.wake:
				continue
			}
		}
		delay, err := l.once(ctx, snap)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			l.log.Warn("FTW Cloud uplink dropped; retrying", "retry_in", delay, "err", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Link) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// allowed reports whether the grant lets a class leave the house.
func (snap grantSnapshot) allowed(class string) bool {
	scope, forwarded := cloudwire.ClassScope[class]
	if !forwarded {
		return false
	}
	return scope == "" || snap.g.has(scope)
}

// caps is the classes this box sends under the grant.
func (snap grantSnapshot) caps() []string {
	var out []string
	for _, c := range []string{cloudwire.MsgLive, cloudwire.MsgLP, cloudwire.MsgPrices, cloudwire.MsgHB, cloudwire.MsgEvent} {
		if snap.allowed(c) {
			out = append(out, c)
		}
	}
	return append(out, cloudwire.MsgRoster)
}

// tlsConfig is TLS 1.3 only, with the system roots unless a test or a dev
// build adds its own.
func (l *Link) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: l.opts.RootCAs}
}
