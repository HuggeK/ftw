package cloudlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

const (
	dialTimeout = 15 * time.Second
	// writeTimeout: a write blocked this long is a dead socket.
	writeTimeout = 10 * time.Second
	// readTimeout is three of the service's 15 s pings.
	readTimeout = 45 * time.Second
	// handshakeTimeout bounds challenge and hello_ok.
	handshakeTimeout = 15 * time.Second
	// purgeWait is how long a revoking box waits for the receipt before it
	// hangs up and tries again later.
	purgeWait = 30 * time.Second

	backoffBase = 500 * time.Millisecond
	backoffCap  = 60 * time.Second
)

// errService is the service breaking the protocol. The link closes and backs
// off; nothing the service says can do more than that.
var errService = errors.New("cloudlink: the service broke the protocol")

func (l *Link) dialer() *websocket.Dialer {
	return &websocket.Dialer{
		// No proxy, whatever the environment says. A proxy that ends TLS
		// breaks the handshake binding by design, and one that does not
		// adds nothing.
		Proxy:             nil,
		HandshakeTimeout:  dialTimeout,
		TLSClientConfig:   l.tlsConfig(),
		EnableCompression: false,
	}
}

// once dials, serves until the connection ends and says how long to wait.
func (l *Link) once(ctx context.Context, snap grantSnapshot) (time.Duration, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, resp, err := l.dialer().DialContext(dialCtx, l.opts.Endpoint, nil)
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return l.backoff(), fmt.Errorf("dialling FTW Cloud: %w", err)
	}
	defer conn.Close()

	connectedAt := time.Now()
	err = l.serve(ctx, conn, snap)
	l.mu.Lock()
	l.connected = false
	if time.Since(connectedAt) >= backoffCap {
		l.attempt = 0
	}
	l.mu.Unlock()
	if errors.Is(err, errPurged) || errors.Is(err, errClosed) {
		return 0, nil
	}
	return l.backoff(), err
}

// backoff is full jitter from 0.5 s to 60 s.
func (l *Link) backoff() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	ceiling := min(backoffBase<<min(l.attempt, 16), backoffCap)
	l.attempt = min(l.attempt+1, 16)
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return ceiling / 2
	}
	return time.Duration(n.Int64())
}

// errPurged ends a connection after the service confirmed a deletion.
var errPurged = errors.New("cloudlink: purge confirmed")

// serve runs one connection: the handshake, then the reader here and the
// sender on its own goroutine.
func (l *Link) serve(ctx context.Context, conn *websocket.Conn, snap grantSnapshot) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ekm, err := exporterValue(conn)
	if err != nil {
		return err
	}
	conn.SetReadLimit(cloudwire.MaxServiceFrame)

	// 1. challenge
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	env, err := readEnvelope(conn)
	if err != nil {
		return err
	}
	var ch cloudwire.Challenge
	if env.T != cloudwire.MsgChallenge || len(env.Sig) != 0 || cloudwire.Unmarshal(env.B, &ch) != nil ||
		ch.V != cloudwire.Version || len(ch.Nonce) != cloudwire.NonceBytes {
		return fmt.Errorf("%w: expected a challenge", errService)
	}

	// 2. hello, the only thing sent before the service has proved itself.
	boxNonce, err := randomBytes(cloudwire.NonceBytes)
	if err != nil {
		return err
	}
	rosterSeq, _ := l.rosterNumber(snap)
	l.mu.Lock()
	hello := cloudwire.Hello{
		Header: l.header(), V: cloudwire.Version, PK: snap.pk, Nonce: ch.Nonce, EKM: ekm,
		BoxNonce: boxNonce, Build: l.opts.Build, Clock: l.clockName(),
		Caps: snap.caps(), RosterSeq: rosterSeq, GrantID: snap.g.ID,
	}
	l.mu.Unlock()
	w := &writer{conn: conn, link: l}
	if err := w.send(snap, cloudwire.MsgHello, hello); err != nil {
		return err
	}

	// 3. hello_ok, signed by a key this build carries, bound to this TLS
	// session and to this hello.
	env, err = readEnvelope(conn)
	if err != nil {
		return err
	}
	var ok cloudwire.HelloOK
	if env.T != cloudwire.MsgHelloOK || !l.verifiedByService(snap.home, env) ||
		cloudwire.Unmarshal(env.B, &ok) != nil ||
		!bytes.Equal(ok.BoxNonce, boxNonce) || !bytes.Equal(ok.EKM, ekm) {
		return fmt.Errorf("%w: hello_ok did not check out", errService)
	}
	if ok.RosterSeq > rosterSeq {
		// A restore from an older backup: move past the service's number
		// so revokes made since then still arrive.
		l.bumpRoster(ok.RosterSeq)
	}

	l.mu.Lock()
	l.connected = true
	l.mu.Unlock()
	l.log.Info("FTW Cloud uplink connected", "home", snap.home)

	events := make(chan cloudwire.Envelope, 4)
	readErr := make(chan error, 1)
	go func() {
		readErr <- l.read(conn, snap, events)
	}()
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- w.run(ctx, events)
	}()

	select {
	case err := <-readErr:
		cancel()
		<-sendErr
		return err
	case err := <-sendErr:
		cancel()
		<-readErr
		return err
	}
}

// exporterValue is the TLS 1.3 exporter value both ends bind the handshake to.
func exporterValue(conn *websocket.Conn) ([]byte, error) {
	tc, ok := conn.NetConn().(*tls.Conn)
	if !ok {
		return nil, errors.New("cloudlink: the uplink is not TLS")
	}
	cs := tc.ConnectionState()
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("cloudlink: the uplink is not TLS 1.3")
	}
	return cs.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
}

func (l *Link) verifiedByService(home string, env cloudwire.Envelope) bool {
	for _, k := range l.opts.ServiceKeys {
		if cloudwire.Verify(k, home, env) == nil {
			return true
		}
	}
	return false
}

func (l *Link) clockName() string {
	if l.clockSynced() {
		return cloudwire.ClockNTP
	}
	return cloudwire.ClockNone
}

// readEnvelope reads one binary frame. A text message, a lane 0 frame or a
// frame over 4 KiB is the service breaking the protocol.
func readEnvelope(conn *websocket.Conn) (cloudwire.Envelope, error) {
	kind, msg, err := conn.ReadMessage()
	if err != nil {
		return cloudwire.Envelope{}, err
	}
	return parseDownlink(kind, msg)
}

func parseDownlink(kind int, msg []byte) (cloudwire.Envelope, error) {
	if kind != websocket.BinaryMessage {
		return cloudwire.Envelope{}, fmt.Errorf("%w: a text message", errService)
	}
	if len(msg) > cloudwire.MaxServiceFrame {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %d bytes", errService, len(msg))
	}
	payload, err := cloudwire.DecodeFrame(msg)
	if err != nil {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %v", errService, err)
	}
	env, err := cloudwire.DecodeEnvelope(payload)
	if err != nil {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %v", errService, err)
	}
	if !slices.Contains(cloudwire.Downlink, env.T) {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %q is not a message the service may send", errService, env.T)
	}
	return env, nil
}

// read is the downlink. After the handshake the service may send watch,
// resync and purged; challenge and hello_ok a second time are refused like
// any stranger. watch and resync go to the sender, which owns the cadence.
// A purge receipt is taken here, before the service hangs up, so the box
// never misses one it was sent.
func (l *Link) read(conn *websocket.Conn, snap grantSnapshot, events chan<- cloudwire.Envelope) error {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
	})
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		switch env.T {
		case cloudwire.MsgPurged:
			if l.acceptPurged(snap, env) {
				return errPurged
			}
		case cloudwire.MsgWatch, cloudwire.MsgResync:
			events <- env
		default:
			return fmt.Errorf("%w: %q after the handshake", errService, env.T)
		}
	}
}

// writer is the one goroutine that writes data frames on a connection.
type writer struct {
	conn *websocket.Conn
	link *Link
	mu   sync.Mutex
}

// send seals body as type t, frames it and writes it. The byte budget is
// the caller's to check.
func (w *writer) send(snap grantSnapshot, t string, body any) error {
	raw, err := cloudwire.Marshal(body)
	if err != nil {
		return err
	}
	env, err := cloudwire.SealBytes(snap.key, snap.home, t, raw)
	if err != nil {
		return err
	}
	payload, err := env.Encode()
	if err != nil {
		return err
	}
	frame, err := cloudwire.EncodeFrame(payload)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	if err := w.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return err
	}
	l := w.link
	l.mu.Lock()
	l.lastSend = time.Now()
	l.budget.add(l.uptimeMs(), int64(len(frame)))
	l.sent[t] = sentFrame{T: t, Frame: frame, AtMs: l.uptimeMs()}
	l.mu.Unlock()
	return nil
}
