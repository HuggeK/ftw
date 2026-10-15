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
)

// session is one app's encrypted conversation with this box.
//
// A session belongs to one relay connection and never outlives it. The app
// tears its own Noise session down whenever its carrier drops, because the
// keys are bound to one handshake and the counters to one stream, so keeping
// state across a reconnect here would only be state the peer has forgotten.
type session struct {
	transport *appwire.Transport
	handler   *appproto.Handler
	// appStatic is the app's static key as the handshake authenticated it,
	// never as it was claimed. Logged as a prefix and nothing more.
	appStatic []byte
	// release frees the device's slot (see slots), if the session still
	// holds it. Nil for a session that took none. Set through setRelease,
	// because a newer session can end this one before its claim returns.
	relMu   sync.Mutex
	release func()
	closed  bool
}

// setRelease keeps the slot's release for close, or runs it at once when the
// session already ended.
func (s *session) setRelease(fn func()) {
	s.relMu.Lock()
	if s.closed {
		s.relMu.Unlock()
		fn()
		return
	}
	s.release = fn
	s.relMu.Unlock()
}

// close ends one session for good.
//
// Both halves matter and neither is enough alone: closing the transport stops
// anything further being encrypted onto the socket, and closing the handler
// cancels work already running inside the box — a passthrough request in
// flight, which must not be allowed to finish for a phone that has just been
// locked out.
func (s *session) close() {
	s.handler.Close()
	s.transport.Close()
	s.relMu.Lock()
	s.closed = true
	fn := s.release
	s.release = nil
	s.relMu.Unlock()
	if fn != nil {
		fn()
	}
}

// pending is a handshake the box answered whose session has not yet proved
// itself. A message 1 can be replayed by whoever carries it, and the box
// cannot tell a replay from a phone until the phone's first transport
// message decrypts. Until then it takes no device's place, stamps nothing
// and spends no code.
type pending struct {
	transport *appwire.Transport
	appStatic []byte
	deviceID  string
	commit    func() (appenroll.Grant, error)
	at        time.Time
}

// pendingKept is how many unproved handshakes a table holds, and how long
// one waits for its first transport message.
const (
	pendingKept = 8
	pendingWait = 30 * time.Second
)

// sender encrypts one session's frames onto the shared socket.
type sender struct {
	transport *appwire.Transport
	write     func([]byte) error
}

func (s sender) Send(frame []byte) error {
	message, err := s.transport.Encrypt(frame)
	if err != nil {
		return err
	}
	return s.write(message)
}

// sessions is the demultiplexer.
//
// The relay broadcasts the box's frames to every stream in the room and hands
// the box every stream's frames without saying which is which — it cannot say,
// because telling them apart would mean reading them. So the box does what the
// app does in the other direction: try each live session, and let the AEAD
// decide. A message nothing authenticates is either a new handshake or noise,
// and noise is dropped.
type sessions struct {
	mu      sync.Mutex
	live    []*session
	pending []*pending
	build   HandlerBuilder
	// slots is the box's one slot per device, shared with the tunnel. Nil
	// in a test that builds a table alone.
	slots *slots

	enroll *appenroll.Identity
	static appwire.KeyPair
	write  func([]byte) error
	log    *slog.Logger

	// testOut is where a test's write puts what the table sends.
	testOut chan []byte
}

// prologue binds a session to the box it was enrolled against, so a captured
// handshake cannot be replayed into a different one. Both ends must spell it
// identically; the app builds it in src/lib/state/connect.ts.
func prologue(boxStatic []byte) []byte {
	tag := []byte("ftw.session.v1:")
	return append(tag, boxStatic...)
}

// deliver routes one message from the relay.
func (s *sessions) deliver(ctx context.Context, message []byte) {
	if s.route(ctx, message) {
		return
	}
	if err := s.open(message); err != nil {
		// Anyone who can reach the relay can write to this room, so a
		// message that authenticates against nothing is ordinary. Tearing
		// the uplink down over one would hand a stranger a way to cut the
		// household off.
		s.log.Debug("message matched no session and opened none", "err", err)
	}
}

// route hands the message to the session that can decrypt it, if any. A
// message that proves a pending handshake makes its session live first.
func (s *sessions) route(ctx context.Context, message []byte) bool {
	s.mu.Lock()
	live := append([]*session(nil), s.live...)
	waiting := append([]*pending(nil), s.pending...)
	s.mu.Unlock()

	for _, p := range waiting {
		frame, err := p.transport.Decrypt(message)
		if err != nil {
			continue
		}
		sess, err := s.promote(p)
		if err != nil {
			s.log.Debug("a proved handshake was not admitted", "err", err)
			return true
		}
		if err := sess.handler.Handle(ctx, frame); err != nil {
			s.log.Warn("app session failed on a frame", "err", err)
			s.drop(sess)
		}
		return true
	}

	for _, sess := range live {
		frame, err := sess.transport.Decrypt(message)
		if err != nil {
			continue
		}
		if err := sess.handler.Handle(ctx, frame); err != nil {
			s.log.Warn("app session failed on a frame", "err", err)
			s.drop(sess)
		}
		return true
	}
	return false
}

// open runs the responder half of a handshake and, if the app is allowed in,
// keeps the session.
func (s *sessions) open(message []byte) error {
	if len(message) < appwire.Message1Overhead {
		return errors.New("appuplink: too short to be handshake message 1")
	}

	handshake, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: s.static,
		Prologue:  prologue(s.static.Public()),
	})
	if err != nil {
		return err
	}

	payload, err := handshake.ReadMessage(message)
	if err != nil {
		return err
	}

	// The reply is built before the caller is checked, because the app's
	// static key is only authenticated once the pattern has run — but it is
	// not sent until the check passes. Replying to a stranger would confirm
	// that a box is on this handle and hand them a fresh ephemeral key for
	// their trouble.
	reply, err := handshake.WriteMessage(nil)
	if err != nil {
		return err
	}

	noise, err := handshake.Split()
	if err != nil {
		return err
	}
	transport := appwire.NewTransport(noise)

	// The payload is the pairing code from the QR. A device that has already
	// paired presents nothing and is recognised by its static key, which is
	// what makes the code single-use without breaking reconnects. Checked
	// now, committed when the session proves itself.
	grant, commit, err := s.enroll.Check(noise.RemoteStatic, payload)
	if err != nil {
		transport.Close()
		return err
	}

	s.mu.Lock()
	now := time.Now()
	kept := s.pending[:0]
	for _, p := range s.pending {
		if now.Sub(p.at) < pendingWait {
			kept = append(kept, p)
		} else {
			p.transport.Close()
		}
	}
	s.pending = kept
	if len(s.pending) >= pendingKept {
		s.pending[0].transport.Close()
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, &pending{transport: transport, appStatic: noise.RemoteStatic,
		deviceID: grant.DeviceID, commit: commit, at: now})
	s.mu.Unlock()

	if err := s.write(reply); err != nil {
		return err
	}
	return nil
}

// promote makes a proved handshake a live session: it commits the pairing or
// the reconnect stamp, builds the handler from the grant as it stands now,
// and takes the device's one slot.
func (s *sessions) promote(p *pending) (*session, error) {
	s.mu.Lock()
	for i, q := range s.pending {
		if q == p {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	grant, err := p.commit()
	if err != nil {
		p.transport.Close()
		return nil, err
	}

	// The grant becomes a caller here, and this is the only place it is
	// built. Everything the box later decides about this phone — which
	// commands it may send, which routes it may reach — is decided from this
	// value, and nothing the phone sends can add to it.
	caller := callerOf(grant)
	handler, err := s.build(
		sender{transport: p.transport, write: s.write},
		caller,
		liveGrant{enroll: s.enroll, deviceID: grant.DeviceID},
	)
	if err != nil {
		p.transport.Close()
		return nil, err
	}

	sess := &session{transport: p.transport, handler: handler, appStatic: p.appStatic}
	s.admit(sess)
	// The device's one slot: a session it held before, here or through FTW
	// Cloud, ends now.
	sess.setRelease(s.slots.claim(grant.DeviceID, func() { s.drop(sess) }))

	s.log.Info("app session open", "sessions", s.count())
	return sess, nil
}

// admit adds a session. There is no count to evict by: each device holds one
// slot (see slots), so a phone that reconnects replaces its own session and
// never another device's. The relay lets four streams into a room.
func (s *sessions) admit(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = append(s.live, sess)
}

// dropByAppKey terminates and forgets every session a revoked key is
// running. Termination is best-effort — the socket may already be gone —
// but the forget is not: a frame from a dropped session no longer
// authenticates against anything.
func (s *sessions) dropByAppKey(pub []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	waiting := s.pending[:0]
	for _, p := range s.pending {
		if bytes.Equal(p.appStatic, pub) {
			p.transport.Close()
			continue
		}
		waiting = append(waiting, p)
	}
	s.pending = waiting

	dropped := 0
	kept := s.live[:0]
	for _, live := range s.live {
		if bytes.Equal(live.appStatic, pub) {
			// The reason the app already knows how to say: its terminated
			// screen reads "your access was withdrawn by its owner".
			_ = live.handler.Terminate(appproto.TerminateRevoked)
			live.close()
			dropped++
			continue
		}
		kept = append(kept, live)
	}
	s.live = kept
	return dropped
}

func (s *sessions) drop(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, live := range s.live {
		if live == sess {
			live.close()
			s.live = append(s.live[:i], s.live[i+1:]...)
			return
		}
	}
}

// closeAll ends every session. Called when the relay says the last app left
// and when the socket dies: in both cases the app has already forgotten its
// half, so holding ours would only be memory nobody can decrypt into.
func (s *sessions) closeAll() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	for _, p := range s.pending {
		p.transport.Close()
	}
	s.pending = nil
	s.mu.Unlock()

	for _, sess := range live {
		sess.close()
	}
}

// liveGrant is one session's view of its own enrolment, as it stands now.
//
// The device id and not the key, because that is what the settings page
// revokes by and what an audit line names — one identifier for the phone
// across every place the box talks about it.
type liveGrant struct {
	enroll   *appenroll.Identity
	deviceID string
}

func (g liveGrant) Grant() (appproto.Enrolment, bool) {
	grant, ok := g.enroll.GrantFor(g.deviceID)
	if !ok {
		return appproto.Enrolment{}, false
	}
	return enrolmentOf(grant), true
}

// enrolmentOf is a grant as the message layer reads it. A watch spends its
// parent phone's command ids, so one intent sent by both acts once. An agent
// spends its own.
func enrolmentOf(grant appenroll.Grant) appproto.Enrolment {
	e := appproto.Enrolment{Role: grant.Role, Epoch: grant.Epoch, ParentRole: grant.ParentRole}
	if grant.Parent != "" {
		e.Root = subjectOf(grant.Parent)
	}
	if a := grant.Agent; a != nil {
		e.Agent = &appproto.AgentGrant{Scopes: a.Scopes, Ops: a.Ops, Caps: a.Caps, ExpiresAtMs: a.ExpiresAtMs,
			Principal: a.Principal, Expired: a.Expired, NoClock: a.NoClock}
	}
	return e
}

// callerOf is the caller a session carries. The grant becomes a caller here
// and nowhere else: everything the box later decides about the session is
// decided from this value, and nothing the device sends can add to it.
func callerOf(grant appenroll.Grant) apiauth.Caller {
	kind := apiauth.KindApp
	if grant.Agent != nil {
		kind = apiauth.KindAgent
	}
	return apiauth.Caller{
		Subject: kind + ":" + grant.DeviceID,
		Kind:    kind,
		Role:    grant.Role,
		Scopes:  appproto.ScopesForEnrolment(enrolmentOf(grant)),
		Epoch:   grant.Epoch,
	}
}

// subjectOf is the caller subject for a device id. Built here and nowhere
// else, so the command log and the audit line name a device the same way.
func subjectOf(deviceID string) string {
	return apiauth.KindApp + ":" + deviceID
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// tick sends the global one-second pulse to every live session.
//
// Each handler gates that pulse to its own subscribed 1 Hz or 0.2 Hz cadence.
// On a due pulse it sends a frame even when nothing changed, so silence within
// that cadence does not reveal whether the house was idle.
func (s *sessions) tick() {
	s.mu.Lock()
	live := append([]*session(nil), s.live...)
	s.mu.Unlock()

	for _, sess := range live {
		if err := sess.handler.Tick(); err != nil {
			s.log.Warn("app session failed on a tick", "err", err)
			s.drop(sess)
		}
	}
}
