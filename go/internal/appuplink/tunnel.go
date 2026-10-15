package appuplink

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The tunnel: the phones' sessions that FTW Cloud carries
// (docs/protocol-cloud.md, "Tunnel").
//
// A phone's session through the service is the same ftw.session.v1 session
// it runs over the relay, byte for byte. Only the routing differs. The relay
// cannot say whose a message is, so the box lets each session's AEAD try. The
// service says which device and which session, tun {dev, sid, d}, so the box
// looks the session up. It still trusts nothing the service says: the
// handshake proves the phone's key and the box admits it by that key, exactly
// as over the relay. dev only picks the slot, and a handshake whose key is
// not the device dev names is dropped.
//
// A new session waits, pending, until its first transport message decrypts.
// The service sees every message 1 and can replay one under a new sid; the
// box answers it, but the replay never proves itself, so it takes no
// device's place, stamps nothing and spends no code. Each new session runs
// its handshake on a goroutine of its own, so a slow disk under the
// enrolment write never holds up routing or the one-second tick.

// TunSender sends one tun up the box's link to FTW Cloud. cloudlink.Link is
// one. bulk says the message carries a lane 1 frame, which the link paces;
// everything else goes first. An error whose Dropped method says true lost
// that one message and counted it; any other error means no message will
// leave until the box reconnects.
type TunSender interface {
	SendTun(t cloudwire.Tun, bulk bool) error
}

// dropped reports whether a TunSender's error lost one message only.
func dropped(err error) bool {
	var d interface{ Dropped() bool }
	return errors.As(err, &d) && d.Dropped()
}

const (
	// tunInbox is how many tun messages wait for the table. The link's
	// reader never waits on it; past it a message is dropped, which Noise
	// transport survives at the cost of one frame.
	tunInbox = 256
	// tunSessionInbox is the same, per session, so one phone's slow
	// command never holds up another phone.
	tunSessionInbox = 64
	// tunOpensPerDevice bounds one device's handshakes through the service
	// a minute, so one phone or one pairing cannot starve the others.
	// tunOpensPerMinute bounds them all, each of which costs the box up to
	// four X25519 operations.
	tunOpensPerDevice = 12
	tunOpensPerMinute = 240
	// tunPendingPerDevice is how many unproved sessions a device may hold,
	// and pendingWait (session.go) how long one waits to prove itself.
	tunPendingPerDevice = 4
)

// Tunnel is the box's table of sessions that FTW Cloud carries: one live per
// device, in the slot that device holds across every carrier, and the
// pending ones that have not yet proved themselves.
type Tunnel struct {
	build  HandlerBuilder
	enroll *appenroll.Identity
	static appwire.KeyPair
	slots  *slots
	log    *slog.Logger
	now    func() time.Time

	inbox chan tunIn

	mu     sync.Mutex
	sender TunSender
	// gen moves on every Drop, so a message queued from an uplink that has
	// ended opens nothing on the next.
	gen     uint64
	live    map[string]*tunSession
	pending map[tunKey]*tunSession
	opens   map[string][]time.Time
}

type tunKey struct {
	dev string
	sid uint32
}

type tunIn struct {
	t   cloudwire.Tun
	gen uint64
}

// tunSession is one phone's session through the service. Its session is nil
// until it proves itself; its goroutine owns everything else.
type tunSession struct {
	dev     string
	sid     uint32
	gen     uint64
	session *session
	// appStatic is the key the handshake proved, once it has run.
	appStatic []byte
	in        chan []byte
	stop      chan struct{}
	once      sync.Once
}

// Tunnel returns the uplink's tunnel table, for the box's link to FTW Cloud
// to deliver to. Its sessions take the same slots as the relay's.
func (u *Uplink) Tunnel() *Tunnel { return u.tunnel }

func newTunnel(build HandlerBuilder, enroll *appenroll.Identity, slots *slots, log *slog.Logger) *Tunnel {
	return &Tunnel{
		build: build, enroll: enroll, static: enroll.StaticKey(), slots: slots, log: log, now: time.Now,
		inbox: make(chan tunIn, tunInbox), live: map[string]*tunSession{}, pending: map[tunKey]*tunSession{},
		opens: map[string][]time.Time{},
	}
}

// Attach names where answers go. The box's link calls nothing until then.
func (t *Tunnel) Attach(s TunSender) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sender = s
}

// Deliver takes one tun from the service. It never blocks.
func (t *Tunnel) Deliver(m cloudwire.Tun) {
	t.mu.Lock()
	gen := t.gen
	t.mu.Unlock()
	select {
	case t.inbox <- tunIn{t: m, gen: gen}:
	default:
		t.log.Debug("tunnel inbox full; a message was dropped")
	}
}

// Drop ends every session, when the uplink that carried them ends. The
// phones start again on the next.
func (t *Tunnel) Drop() {
	t.mu.Lock()
	t.gen++
	all := make([]*tunSession, 0, len(t.live)+len(t.pending))
	for _, s := range t.live {
		all = append(all, s)
	}
	for _, s := range t.pending {
		all = append(all, s)
	}
	t.live, t.pending = map[string]*tunSession{}, map[tunKey]*tunSession{}
	t.mu.Unlock()
	for _, s := range all {
		s.end()
	}
}

// Run serves the table until ctx ends: the messages the service delivers,
// and the one-second pulse lane 0 needs.
func (t *Tunnel) Run(ctx context.Context) {
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	defer t.Drop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-t.inbox:
			t.route(ctx, m)
		case <-tick.C:
			t.tick()
		}
	}
}

// count is how many live sessions the table holds.
func (t *Tunnel) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// route hands one message to its session, live or pending, or starts a
// pending one. It never waits: each session works on its own goroutine.
func (t *Tunnel) route(ctx context.Context, m tunIn) {
	key := tunKey{m.t.Dev, m.t.Sid}
	t.mu.Lock()
	if m.gen != t.gen {
		t.mu.Unlock()
		return
	}
	s := t.pending[key]
	if cur := t.live[m.t.Dev]; cur != nil && cur.sid == m.t.Sid {
		s = cur
	}
	if s == nil && !m.t.Ended() {
		s = t.startLocked(ctx, key, m.gen)
	}
	t.mu.Unlock()

	switch {
	case s == nil:
		// The end of a session the box no longer holds, or a handshake
		// over the device's allowance.
	case m.t.Ended():
		t.remove(s, false)
	default:
		select {
		case s.in <- m.t.D:
		default:
			t.log.Debug("tunnel session inbox full; a message was dropped")
		}
	}
}

// startLocked makes a pending session for a new (dev, sid), within the
// device's allowance. The caller holds t.mu.
func (t *Tunnel) startLocked(ctx context.Context, key tunKey, gen uint64) *tunSession {
	if !t.allowOpenLocked(key.dev) {
		return nil
	}
	held := 0
	var oldest *tunSession
	for k, p := range t.pending {
		if k.dev == key.dev {
			held++
			if oldest == nil || p.sid != key.sid {
				oldest = p
			}
		}
	}
	if held >= tunPendingPerDevice && oldest != nil {
		delete(t.pending, tunKey{oldest.dev, oldest.sid})
		go oldest.end()
	}
	s := &tunSession{dev: key.dev, sid: key.sid, gen: gen,
		in: make(chan []byte, tunSessionInbox), stop: make(chan struct{})}
	t.pending[key] = s
	go s.run(ctx, t)
	return s
}

var (
	errTunDevice = errors.New("appuplink: the handshake's key is not the device the service named")
	errTunShort  = errors.New("appuplink: too short to be handshake message 1")
	errTunGone   = errors.New("appuplink: the tunnel's uplink is gone")
)

// run is one session's life: the handshake on its first message, then each
// message in order. Until a transport message decrypts, the session is
// pending and has no handler; the first that does makes it live.
func (s *tunSession) run(ctx context.Context, t *Tunnel) {
	var first []byte
	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case first = <-s.in:
	}
	transport, commit, err := t.handshake(s, first)
	if err != nil {
		t.log.Debug("tun opened no session", "err", err)
		t.remove(s, false)
		return
	}
	defer transport.Close()

	wait := time.NewTimer(pendingWait)
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-wait.C:
			if s.session == nil {
				// Never proved: a replay, or a phone that went away.
				t.remove(s, false)
				return
			}
		case d := <-s.in:
			frame, err := transport.Decrypt(d)
			if err != nil {
				// A replay, or bytes that are not this session's. The
				// service can delay and drop; it cannot make the box act.
				continue
			}
			if s.session == nil {
				if err := t.promote(s, transport, commit); err != nil {
					t.log.Debug("a proved tunnel session was not admitted", "err", err)
					t.remove(s, true)
					return
				}
			}
			if err := s.session.handler.Handle(ctx, frame); err != nil {
				t.log.Warn("app session failed on a frame", "err", err)
				t.remove(s, true)
				return
			}
		}
	}
}

// handshake runs the responder half for a new session and answers it. A
// message that is not a message 1 gets an end back, so the phone starts
// again; a key the box does not admit, or one the service put under another
// device, gets nothing.
func (t *Tunnel) handshake(s *tunSession, message1 []byte) (*appwire.Transport, func() (appenroll.Grant, error), error) {
	end := cloudwire.Tun{Dev: s.dev, Sid: s.sid}
	if len(message1) < appwire.Message1Overhead {
		t.send(end)
		return nil, nil, errTunShort
	}
	hs, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: t.static,
		Prologue:  prologue(t.static.Public()),
	})
	if err != nil {
		return nil, nil, err
	}
	payload, err := hs.ReadMessage(message1)
	if err != nil {
		// Not message 1 of this box's session: most often a frame of a
		// session the box dropped. The phone starts again.
		t.send(end)
		return nil, nil, err
	}
	// The service names the device; the handshake proves it. A key that is
	// not the named device's is the service, or a bug, putting one phone's
	// session under another's slot.
	if cloudwire.DeviceID(hs.RemoteStatic()) != s.dev {
		return nil, nil, errTunDevice
	}
	// Built before the caller is checked and sent only after, as over the
	// relay. The check changes nothing yet; commit does, once the session
	// proves itself.
	reply, err := hs.WriteMessage(nil)
	if err != nil {
		return nil, nil, err
	}
	noise, err := hs.Split()
	if err != nil {
		return nil, nil, err
	}
	transport := appwire.NewTransport(noise)
	if _, commit, err := t.enroll.Check(noise.RemoteStatic, payload); err != nil {
		transport.Close()
		return nil, nil, err
	} else {
		t.mu.Lock()
		s.appStatic = noise.RemoteStatic
		t.mu.Unlock()
		if !t.send(cloudwire.Tun{Dev: s.dev, Sid: s.sid, D: reply}) {
			transport.Close()
			return nil, nil, errTunGone
		}
		return transport, commit, nil
	}
}

// promote makes a proved session live: it commits the pairing or the
// reconnect stamp, builds the handler from the grant as it stands now, takes
// the device's one slot and the table's live place.
func (t *Tunnel) promote(s *tunSession, transport *appwire.Transport, commit func() (appenroll.Grant, error)) error {
	grant, err := commit()
	if err != nil {
		return err
	}
	caller := apiauth.Caller{
		Subject: subjectOf(grant.DeviceID),
		Kind:    apiauth.KindApp,
		Role:    grant.Role,
		Scopes:  appproto.ScopesForGrant(grant.Role, grant.ParentRole),
		Epoch:   grant.Epoch,
	}
	send := func(msg []byte, bulk bool) error {
		return t.sendTun(cloudwire.Tun{Dev: s.dev, Sid: s.sid, D: msg}, bulk)
	}
	handler, err := t.build(tunSender{transport: transport, send: send}, caller,
		liveGrant{enroll: t.enroll, deviceID: grant.DeviceID})
	if err != nil {
		return err
	}
	sess := &session{transport: transport, handler: handler, appStatic: s.appStatic}

	t.mu.Lock()
	if s.gen != t.gen {
		// The uplink ended while the session proved itself.
		t.mu.Unlock()
		sess.close()
		return errTunGone
	}
	delete(t.pending, tunKey{s.dev, s.sid})
	old := t.live[s.dev]
	s.session = sess
	t.live[s.dev] = s
	t.mu.Unlock()
	if old != nil && old != s {
		// The phone hears that its older session ended.
		t.remove(old, true)
	}
	// The device's one slot, whichever carrier held it before. Claiming it
	// ends the older session, through the relay or through here.
	sess.setRelease(t.slots.claim(grant.DeviceID, func() { t.remove(s, true) }))
	t.log.Info("app session open through FTW Cloud")
	return nil
}

// tunSender encrypts one session's frames for the tunnel and says which are
// bulk, so the link paces those and sends lane 0 (commands, their answers
// and the ticks) first. A message the link dropped and counted is gone like
// any lost frame, and the session goes on; only a lost uplink fails it.
type tunSender struct {
	transport *appwire.Transport
	send      func(msg []byte, bulk bool) error
}

func (s tunSender) Send(frame []byte) error {
	msg, err := s.transport.Encrypt(frame)
	if err != nil {
		return err
	}
	return s.send(msg, len(frame) > 1 && frame[1] == appwire.LaneBulk)
}

// end closes the session once and frees its slot. It reports whether this
// call ended it.
func (s *tunSession) end() bool {
	ended := false
	s.once.Do(func() {
		ended = true
		close(s.stop)
		if s.session != nil {
			s.session.close()
		}
	})
	return ended
}

// remove takes a session out of the table and ends it. tell sends the phone
// an end, when the uplink that carried it is still up.
func (t *Tunnel) remove(s *tunSession, tell bool) {
	t.mu.Lock()
	if t.live[s.dev] == s {
		delete(t.live, s.dev)
	}
	if t.pending[tunKey{s.dev, s.sid}] == s {
		delete(t.pending, tunKey{s.dev, s.sid})
	}
	current := s.gen == t.gen
	t.mu.Unlock()
	if s.end() && tell && current {
		t.send(cloudwire.Tun{Dev: s.dev, Sid: s.sid})
	}
}

// send hands one tun to the link, ahead of bulk: a handshake's answer and
// an end. False means it did not leave.
func (t *Tunnel) send(m cloudwire.Tun) bool { return t.sendTun(m, false) == nil }

// sendTun hands a session's message to the link. A counted drop is not an
// error: Noise transport survives it, and failing the session would lose a
// command id it holds. A lost uplink is.
func (t *Tunnel) sendTun(m cloudwire.Tun, bulk bool) error {
	t.mu.Lock()
	s := t.sender
	t.mu.Unlock()
	if s == nil {
		return errTunGone
	}
	err := s.SendTun(m, bulk)
	if dropped(err) {
		return nil
	}
	return err
}

// allowOpenLocked counts a handshake against its device's allowance for the
// minute, and against the box's. The caller holds t.mu.
func (t *Tunnel) allowOpenLocked(dev string) bool {
	now := t.now()
	total := 0
	for d, list := range t.opens {
		kept := list[:0]
		for _, at := range list {
			if now.Sub(at) < time.Minute {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(t.opens, d)
			continue
		}
		t.opens[d] = kept
		total += len(kept)
	}
	if total >= tunOpensPerMinute || len(t.opens[dev]) >= tunOpensPerDevice {
		return false
	}
	t.opens[dev] = append(t.opens[dev], now)
	return true
}

// tick sends the one-second pulse to every live session.
func (t *Tunnel) tick() {
	t.mu.Lock()
	live := make([]*tunSession, 0, len(t.live))
	for _, s := range t.live {
		live = append(live, s)
	}
	t.mu.Unlock()
	for _, s := range live {
		if err := s.session.handler.Tick(); err != nil {
			t.log.Warn("app session failed on a tick", "err", err)
			t.remove(s, true)
		}
	}
}

// dropByAppKey ends every session a revoked key runs, live or pending, as
// the relay's table does.
func (t *Tunnel) dropByAppKey(pub []byte) int {
	t.mu.Lock()
	var gone []*tunSession
	for _, s := range t.live {
		if bytes.Equal(s.appStatic, pub) {
			gone = append(gone, s)
		}
	}
	for _, s := range t.pending {
		if bytes.Equal(s.appStatic, pub) {
			gone = append(gone, s)
		}
	}
	t.mu.Unlock()
	dropped := 0
	for _, s := range gone {
		if s.session != nil {
			_ = s.session.handler.Terminate(appproto.TerminateRevoked)
			dropped++
		}
		t.remove(s, true)
	}
	return dropped
}

// --------------------------------------------------------------------------
// One slot per device
// --------------------------------------------------------------------------

// slots holds each device's one session, whichever carrier it rides. A new
// session from a device ends its older one, so a phone that reconnects, a
// watch or another phone never pushes out a different device's session.
type slots struct {
	mu   sync.Mutex
	n    uint64
	held map[string]slot
}

type slot struct {
	id  uint64
	end func()
}

// claim gives dev's slot to a new session, whose end func closes it, and
// ends the session that held it. The returned func frees the slot if this
// session still holds it. A nil table claims nothing.
func (s *slots) claim(dev string, end func()) func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.held == nil {
		s.held = map[string]slot{}
	}
	s.n++
	id := s.n
	old, had := s.held[dev]
	s.held[dev] = slot{id: id, end: end}
	s.mu.Unlock()
	if had {
		old.end()
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur, ok := s.held[dev]; ok && cur.id == id {
			delete(s.held, dev)
		}
	}
}
