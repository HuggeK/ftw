package cloudlink

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The uplink over QUIC against a service that speaks it (docs/protocol-cloud.md,
// "QUIC"): the race with the WebSocket, the bindings, and how each message
// travels.

// serveQUIC makes the fake service answer QUIC on the UDP port that matches
// its TCP one, with the same handshake and loop as its WebSocket. A dropping
// service reads every packet and answers none, as a network that blocks UDP.
func (s *fakeService) serveQUIC(dropping bool) string { return s.serveQUICWith(dropping, false) }

// serveQUICWith is serveQUIC with early data on when early, and each
// connection's use of it kept in usedEarly.
func (s *fakeService) serveQUICWith(dropping, early bool) string {
	s.t.Helper()
	addr := s.srv.Listener.Addr().String()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = pc.Close() })
	if dropping {
		go func() {
			buf := make([]byte, 2048)
			for {
				if _, _, err := pc.ReadFrom(buf); err != nil {
					return
				}
			}
		}()
		return addr
	}
	tr := &quic.Transport{Conn: pc}
	s.t.Cleanup(func() { _ = tr.Close() })
	tc := s.srv.TLS.Clone()
	tc.NextProtos = []string{cloudwire.ALPNBox}
	conf := &quic.Config{MaxIdleTimeout: cloudwire.QUICIdleTimeout, EnableDatagrams: true, Allow0RTT: early}
	ln, err := tr.ListenEarly(tc, conf)
	if err != nil {
		s.t.Fatal(err)
	}
	go func() {
		for {
			qc, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				ctrl, err := qc.AcceptStream(context.Background())
				if err != nil {
					return
				}
				r := bufio.NewReader(ctrl)
				target, err := cloudwire.ReadMessage(r, cloudwire.MaxTarget)
				if err != nil || string(target) != cloudwire.TargetBox {
					s.t.Errorf("the box's target: %q, %v", target, err)
					return
				}
				c := &quicConn{qc: qc, ctrl: ctrl, r: r, in: make(chan inbound), kick: make(chan struct{}, 1),
					limit: cloudwire.MaxTunFrame}
				// The exporter exists once the server's flight is out, and
				// hello can only come after the handshake.
				<-qc.HandshakeComplete()
				s.mu.Lock()
				s.usedEarly = append(s.usedEarly, qc.ConnectionState().Used0RTT)
				s.mu.Unlock()
				ekm, _ := c.exporter()
				s.serve(c, ekm)
			}()
		}
	}()
	return addr
}

// A box that dials again sends its target as early data, with the ticket
// from the first connection; hello still binds to the TLS session it rides.
func TestAReconnectSendsItsTargetEarly(t *testing.T) {
	svc := newFakeService(t)
	svc.serveQUICWith(false, true)
	svc.kick = 1
	h := newQUICHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	waitWithin(t, 10*time.Second, "a second connection", func() bool { return svc.count(cloudwire.TransportQUIC) >= 2 })
	waitUntil(t, "the uplink up", func() bool { return h.link.Status().Connected })
	svc.mu.Lock()
	early := slices.Clone(svc.usedEarly)
	svc.mu.Unlock()
	if len(early) < 2 || early[0] || !early[1] {
		t.Fatalf("early data per connection: %v, want the second only", early)
	}
	if got := h.link.Status().Transport; got != cloudwire.TransportQUIC {
		t.Fatalf("transport %q", got)
	}
}

func (s *fakeService) kindsOf(t string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.kinds[t])
}

func (s *fakeService) count(transport string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transports[transport]
}

// newQUICHarness is a box that races QUIC against the WebSocket.
func newQUICHarness(t *testing.T, svc *fakeService) *harness {
	t.Helper()
	h := &harness{svc: svc, site: &fakeSite{grid: 1200, state: protocol.SourceLive}, kv: &memKV{},
		keyPath: filepath.Join(t.TempDir(), "cloud.key"), pushed: make(chan string, 8),
		roster: &fakeRosterState{}, tunnel: &fakeTunnel{ch: make(chan cloudwire.Tun, 64)}}
	link, err := New(Options{
		Site: h.site, Roster: fakeRoster{state: h.roster}, KV: h.kv, KeyPath: h.keyPath, Build: "test",
		Started: time.Now().Add(-time.Minute), ClockSynced: func() bool { return true },
		Notify: func(kind string) { h.pushed <- kind }, Tunnel: h.tunnel,
		Endpoint: svc.endpoint(), QUIC: true, ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey},
		RootCAs: svc.roots(), Tick: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.link = link
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- link.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitWithin(t, 5*time.Second, what, ok)
}

func waitWithin(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Where UDP gets through, the uplink rides QUIC: hello binds to the TLS
// session inside it (the fake service checks), the status says quic, hb and
// a live without its table go as datagrams, and a live with it and the
// roster on the control stream.
func TestTheUplinkRidesQUIC(t *testing.T) {
	svc := newFakeService(t)
	svc.serveQUIC(false)
	h := newQUICHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	if got := h.link.Status(); !got.Connected || got.Transport != cloudwire.TransportQUIC {
		t.Fatalf("status %+v", got)
	}
	if svc.count(cloudwire.TransportWebSocket) != 0 {
		t.Fatal("the WebSocket was dialled while QUIC opened at once")
	}
	// Without a watch a changed live goes within the 5 s period.
	h.site.setGrid(2400)
	waitWithin(t, 12*time.Second, "a live without its table", func() bool { return len(svc.kindsOf(cloudwire.MsgLive)) >= 2 })
	svc.waitFor(cloudwire.MsgHB, 20*time.Second)
	lives := svc.kindsOf(cloudwire.MsgLive)
	if lives[0] != websocket.BinaryMessage || lives[1] != msgDatagram {
		t.Fatalf("live kinds %v: the first carries its table, the next does not", lives)
	}
	if hb := svc.kindsOf(cloudwire.MsgHB); hb[0] != msgDatagram {
		t.Fatalf("hb kinds %v", hb)
	}
	if r := svc.kindsOf(cloudwire.MsgRoster); r[0] != websocket.BinaryMessage {
		t.Fatalf("roster kinds %v", r)
	}
}

// Where UDP is blocked, the WebSocket starts after the race delay and
// carries the uplink; the next connection on the same network starts with
// it.
func TestTheWebSocketTakesOverWhenUDPIsBlocked(t *testing.T) {
	svc := newFakeService(t)
	svc.serveQUIC(true)
	h := newQUICHarness(t, svc)
	start := time.Now()
	h.grant(t, protocol.ScopeLiveRead)
	waitUntil(t, "the uplink up", func() bool { return h.link.Status().Connected })
	took := time.Since(start)
	if got := h.link.Status().Transport; got != cloudwire.TransportWebSocket {
		t.Fatalf("transport %q", got)
	}
	if took < cloudwire.RaceDelay {
		t.Fatalf("the WebSocket opened %v in, before QUIC had its lead", took)
	}
	addr, _ := h.link.quicAddr()
	if got := h.link.preferred(localNetwork(addr)); got != cloudwire.TransportWebSocket {
		t.Fatalf("the race remembered %q", got)
	}

	// The same network again: the WebSocket goes first, with no lead to wait.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	again := time.Now()
	conn, err := h.link.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if transportOf(conn) != cloudwire.TransportWebSocket || time.Since(again) >= cloudwire.RaceDelay {
		t.Fatalf("redial: %s after %v", transportOf(conn), time.Since(again))
	}
}

// A slow QUIC path loses the race to the WebSocket, then opens: the uplink
// stays on the WebSocket, and QUIC goes first on this network next time.
func TestALateQUICTeachesThatUDPGetsThrough(t *testing.T) {
	svc := newFakeService(t)
	// The QUIC listener sits behind a proxy on the TCP port's UDP twin that
	// holds every packet 400 ms, so QUIC opens well after the WebSocket.
	back, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = back.Close() })
	tr := &quic.Transport{Conn: back}
	t.Cleanup(func() { _ = tr.Close() })
	tc := svc.srv.TLS.Clone()
	tc.NextProtos = []string{cloudwire.ALPNBox}
	ln, err := tr.Listen(tc, &quic.Config{MaxIdleTimeout: cloudwire.QUICIdleTimeout, EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	front, err := net.ListenPacket("udp", svc.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Close() })
	go slowProxy(front, back.LocalAddr(), 400*time.Millisecond)

	h := newQUICHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	waitUntil(t, "the uplink up", func() bool { return h.link.Status().Connected })
	if got := h.link.Status().Transport; got != cloudwire.TransportWebSocket {
		t.Fatalf("the slow path carried the uplink: %q", got)
	}
	addr, _ := h.link.quicAddr()
	waitWithin(t, 5*time.Second, "QUIC learned", func() bool {
		return h.link.preferred(localNetwork(addr)) == cloudwire.TransportQUIC
	})
}

// slowProxy forwards UDP between clients of front and back, holding each
// packet d.
func slowProxy(front net.PacketConn, back net.Addr, d time.Duration) {
	var mu sync.Mutex
	ups := map[string]net.PacketConn{}
	buf := make([]byte, 2048)
	for {
		n, from, err := front.ReadFrom(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		mu.Lock()
		up := ups[from.String()]
		if up == nil {
			if up, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
				mu.Unlock()
				continue
			}
			ups[from.String()] = up
			go func(up net.PacketConn, client net.Addr) {
				b := make([]byte, 2048)
				for {
					n, _, err := up.ReadFrom(b)
					if err != nil {
						return
					}
					p := append([]byte(nil), b[:n]...)
					time.AfterFunc(d, func() { _, _ = front.WriteTo(p, client) })
				}
			}(up, from)
		}
		mu.Unlock()
		time.AfterFunc(d, func() { _, _ = up.WriteTo(pkt, back) })
	}
}

// A tun carrying bulk goes on the bulk stream; lane 0 stays on the control
// stream.
func TestBulkTunTakesTheBulkStream(t *testing.T) {
	svc := newFakeService(t)
	svc.serveQUIC(false)
	h := newQUICHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	waitUntil(t, "the uplink up", func() bool { return h.link.Status().Connected })
	waitUntil(t, "the tunnel's queues", func() bool {
		return h.link.SendTun(cloudwire.Tun{Dev: "phone", Sid: 1, D: []byte{1}}, false) == nil
	})
	if err := h.link.SendTun(cloudwire.Tun{Dev: "phone", Sid: 1, D: make([]byte, 4096)}, true); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "two tuns", func() bool { return len(svc.kindsOf(cloudwire.MsgTun)) >= 2 })
	// The two travel on two streams, which QUIC keeps in no order between
	// them, so each is told by its size rather than by which came first.
	bySize := svc.tunKindsBySize()
	if bySize[1] != websocket.BinaryMessage || bySize[4096] != msgBulk {
		t.Fatalf("tun kinds by size %v: lane 0 should come by the control stream (%d), bulk by the bulk stream (%d)",
			bySize, websocket.BinaryMessage, msgBulk)
	}
}

// tunKindsBySize is how each tun from the box came, by the size of its d.
func (s *fakeService) tunKindsBySize() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]int{}
	n := 0
	for _, env := range s.got {
		if env.T != cloudwire.MsgTun {
			continue
		}
		var tun cloudwire.Tun
		if cloudwire.Unmarshal(env.B, &tun) == nil && n < len(s.kinds[cloudwire.MsgTun]) {
			out[len(tun.D)] = s.kinds[cloudwire.MsgTun][n]
		}
		n++
	}
	return out
}

// A datagram from the service, or anything but tun on its bulk stream,
// breaks the protocol, and the box closes.
func TestTheBoxRefusesDatagramsFromTheService(t *testing.T) {
	for _, kind := range []int{msgDatagram, msgBulk} {
		if _, err := parseDownlink(kind, mustFrame(t, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})); err == nil {
			t.Fatalf("a watch as message type %d was taken", kind)
		}
	}
	if _, err := parseDownlink(msgBulk, mustFrame(t, cloudwire.MsgTun, cloudwire.Tun{Dev: "p", Sid: 1, D: []byte{1}})); err != nil {
		t.Fatalf("a tun on the bulk stream: %v", err)
	}
}

func mustFrame(t *testing.T, typ string, body any) []byte {
	t.Helper()
	b, err := cloudwire.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := cloudwire.Envelope{T: typ, B: b}.Encode()
	frame, _ := cloudwire.EncodeFrame(raw)
	return frame
}

// countingConn counts the bytes and packets a UDP socket moves.
type countingConn struct {
	net.PacketConn
	mu                 sync.Mutex
	sent, recv         int
	sentPkts, recvPkts int
}

func (c *countingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.mu.Lock()
	c.sent += n
	c.sentPkts++
	c.mu.Unlock()
	return n, err
}

func (c *countingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.mu.Lock()
	c.recv += n
	c.recvPkts++
	c.mu.Unlock()
	return n, addr, err
}

// What counts as QUIC failing on a network, and what does not.
func TestWhatCountsAsQUICFailing(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		up    bool
		upFor time.Duration
		want  bool
	}{
		{"closed before hello_ok, below 1000", &websocket.CloseError{Code: 0x110}, false, 0, true},
		{"a path error before hello_ok", errors.New("timeout: no recent network activity"), false, 0, true},
		{"the path dropped it soon after", errors.New("timeout: no recent network activity"), true, 10 * time.Second, true},
		{"the path dropped it after a while", errors.New("timeout: no recent network activity"), true, 2 * time.Minute, false},
		{"the service restarts", &websocket.CloseError{Code: cloudwire.CloseRestart}, true, time.Second, false},
		{"the service refuses on purpose", &websocket.CloseError{Code: cloudwire.CloseDenied}, false, 0, false},
		{"a reset after a restart", &quic.StatelessResetError{}, true, time.Second, false},
		{"the link stopped", errLinkEnded, false, 0, false},
	} {
		if got := quicFailed(c.err, c.up, c.upFor); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A service whose QUIC side finishes the handshake and then closes before
// the challenge, as a door with the sessions turned off once did, does not
// hold the box on QUIC: the box goes over to the WebSocket, and QUIC stays
// second on this network even when a QUIC dial wins again.
func TestAQUICSessionThatEndsBeforeHelloOKDemotesQUIC(t *testing.T) {
	svc := newFakeService(t)
	addr := svc.srv.Listener.Addr().String()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	tr := &quic.Transport{Conn: pc}
	t.Cleanup(func() { _ = tr.Close() })
	tc := svc.srv.TLS.Clone()
	tc.NextProtos = []string{cloudwire.ALPNBox}
	ln, err := tr.Listen(tc, &quic.Config{MaxIdleTimeout: cloudwire.QUICIdleTimeout})
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int32
	go func() {
		for {
			qc, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			refused.Add(1)
			_ = qc.CloseWithError(0x110, "")
		}
	}()

	h := newQUICHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	waitWithin(t, 20*time.Second, "the uplink up", func() bool { return h.link.Status().Connected })
	if got := h.link.Status().Transport; got != cloudwire.TransportWebSocket {
		t.Fatalf("transport %q", got)
	}
	if refused.Load() == 0 {
		t.Fatal("QUIC was never tried")
	}
	network := localNetwork(addr)
	if got := h.link.preferred(network); got != cloudwire.TransportWebSocket {
		t.Fatalf("after QUIC failed, %q goes first", got)
	}
	h.link.rememberWinner(network, cloudwire.TransportQUIC)
	if got := h.link.preferred(network); got != cloudwire.TransportWebSocket {
		t.Fatalf("a QUIC win moved %q first within the day", got)
	}
}
