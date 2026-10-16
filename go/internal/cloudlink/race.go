package cloudlink

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The race (docs/protocol-cloud.md, "Racing the WebSocket"): QUIC starts
// first; after cloudwire.RaceDelay without a QUIC handshake the WebSocket
// starts too, and the first to open carries the uplink. The link remembers
// the winner for each network, named by the box's local address towards the
// service, and starts the next time with it, for a day: a network that
// blocked UDP once gets QUIC first again after that.

// winnerHolds is how long a network's winner goes first.
const winnerHolds = 24 * time.Hour

type winner struct {
	transport string
	at        time.Time
	// demoted marks a network where QUIC failed after its handshake (see
	// quicFailed): the WebSocket goes first, and no QUIC win moves QUIC
	// back until the entry is a day old.
	demoted bool
}

type dialed struct {
	transport string
	conn      uplinkConn
	err       error
}

// dial opens the uplink by whichever transport opens first.
func (l *Link) dial(ctx context.Context) (uplinkConn, error) {
	addr, quicOn := l.quicAddr()
	if !quicOn {
		return l.dialWebSocket(ctx)
	}
	network := localNetwork(addr)
	first, second := cloudwire.TransportQUIC, cloudwire.TransportWebSocket
	if l.preferred(network) == cloudwire.TransportWebSocket {
		first, second = second, first
	}

	// QUIC dials on a context of its own: when the WebSocket wins, a QUIC
	// dial still under way goes on for learnFor, only to learn whether UDP
	// gets through here. One lost packet should not keep QUIC second for a
	// day.
	wsCtx, cancelWS := context.WithCancel(ctx)
	// Not ctx itself: the caller ends ctx as soon as dial returns, which
	// would end the dial that is still learning.
	quicCtx, cancelQUIC := context.WithTimeout(context.WithoutCancel(ctx), dialTimeout)
	results := make(chan dialed, 2)
	start := func(transport string) {
		go func() {
			var conn uplinkConn
			var err error
			if transport == cloudwire.TransportQUIC {
				var q *quicConn
				if q, err = dialQUIC(quicCtx, addr, l.quicTLSConfig(), nil); err == nil {
					q.network = network
					conn = q
				}
			} else {
				conn, err = l.dialWebSocket(wsCtx)
			}
			results <- dialed{transport: transport, conn: conn, err: err}
		}()
	}
	start(first)
	started := 1
	lead := time.NewTimer(cloudwire.RaceDelay)
	defer lead.Stop()
	var errs []error
	for {
		select {
		case <-lead.C:
			if started == 1 {
				start(second)
				started = 2
			}
		case r := <-results:
			if r.err == nil {
				l.rememberWinner(network, r.transport)
				if started == 2 {
					l.afterRace(network, r.transport, results, cancelQUIC)
				} else {
					cancelQUIC()
				}
				cancelWS()
				return r.conn, nil
			}
			errs = append(errs, r.err)
			if started == 1 {
				start(second)
				started = 2
				continue
			}
			if len(errs) == 2 {
				cancelQUIC()
				cancelWS()
				return nil, errors.Join(errs...)
			}
		}
	}
}

// learnFor is how long a QUIC dial that lost goes on.
const learnFor = 3 * time.Second

// afterRace waits for the loser. A QUIC dial that opens within learnFor
// teaches that UDP gets through here, so QUIC goes first next time; either
// way the loser is closed.
func (l *Link) afterRace(network, won string, results <-chan dialed, cancelQUIC context.CancelFunc) {
	go func() {
		t := time.NewTimer(learnFor)
		defer t.Stop()
		defer cancelQUIC()
		select {
		case late := <-results:
			if late.err == nil {
				if late.transport == cloudwire.TransportQUIC && won == cloudwire.TransportWebSocket {
					l.rememberWinner(network, cloudwire.TransportQUIC)
				}
				_ = late.conn.Close()
			}
		case <-t.C:
			cancelQUIC()
			if late := <-results; late.err == nil {
				_ = late.conn.Close()
			}
		}
	}()
}

func (l *Link) dialWebSocket(ctx context.Context) (uplinkConn, error) {
	conn, resp, err := l.dialer().DialContext(ctx, l.endpoint(), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// quicAddr is the service's QUIC address: the WebSocket endpoint's host and
// port, over UDP, on the shard that holds the home. False when QUIC is off:
// a test that names its own endpoint and does not ask for QUIC.
func (l *Link) quicAddr() (string, bool) {
	if !l.quicOn {
		return "", false
	}
	if l.opts.QUICAddr != "" {
		return l.opts.QUICAddr, true
	}
	u, err := url.Parse(l.endpoint())
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

// localNetwork names the network the box reaches addr through: its local
// address on the route there. Connecting a UDP socket sends nothing.
func localNetwork(addr string) string {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// preferred is the transport that won last on network, QUIC when none has.
func (l *Link) preferred(network string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.winners[network]; ok && time.Since(w.at) < winnerHolds {
		return w.transport
	}
	return cloudwire.TransportQUIC
}

func (l *Link) rememberWinner(network, transport string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.winners == nil {
		l.winners = map[string]winner{}
	}
	if w, ok := l.winners[network]; ok && w.demoted && time.Since(w.at) < winnerHolds {
		return
	}
	l.winners[network] = winner{transport: transport, at: time.Now()}
}

// demote puts the WebSocket first on network for a day, whatever a QUIC
// dial wins there meanwhile.
func (l *Link) demote(network string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.winners == nil {
		l.winners = map[string]winner{}
	}
	l.winners[network] = winner{transport: cloudwire.TransportWebSocket, at: time.Now(), demoted: true}
}

// quicFailed reports whether a QUIC uplink that ended with err counts as QUIC
// failing on its network (docs/protocol-cloud.md, "Racing the WebSocket"): a
// handshake that finished proves less than a session that works. It counts
// when the uplink ended before hello_ok, or when the path dropped it within
// QUICProvenAfter of it. A close the service sent on purpose, with a code
// from 1000 up, and a stateless reset after a restart do not count.
func quicFailed(err error, up bool, upFor time.Duration) bool {
	if err == nil || errors.Is(err, errLinkEnded) || errors.Is(err, errClosed) {
		return false
	}
	if closed, ok := errors.AsType[*websocket.CloseError](err); ok && closed.Code >= 1000 {
		return false
	}
	if _, ok := errors.AsType[*quic.StatelessResetError](err); ok {
		return false
	}
	return !up || upFor < cloudwire.QUICProvenAfter
}
