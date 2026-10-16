package cloudlink

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// quicConn is the uplink over QUIC: the WebSocket's messages on a control
// stream that starts with the target, bulk tun on a unidirectional stream,
// and hb and live as QUIC DATAGRAMs (docs/protocol-cloud.md, "QUIC").
//
// QUIC stands in for the service's WebSocket ping. The box sends a QUIC
// PING after cloudwire.QUICKeepAlive of quiet, which keeps the NAT's UDP
// mapping open, and the connection ends after cloudwire.QUICIdleTimeout with
// nothing from the service. While it lives, quicConn calls the ping handler
// on the keepalive's period, as the service's pings would.
type quicConn struct {
	qc *quic.Conn
	tr *quic.Transport
	// network is where the race dialled it, for demote.
	network string
	ctrl    *quic.Stream
	r       *bufio.Reader

	start sync.Once
	in    chan inbound
	kick  chan struct{}

	mu       sync.Mutex
	deadline time.Time
	writeDL  time.Time
	limit    int64
	ping     func(string) error
	readErr  error

	writeMu sync.Mutex
	bulkMu  sync.Mutex
	bulk    *quic.SendStream
	noBulk  bool
	closed  sync.Once
}

type inbound struct {
	kind int
	msg  []byte
}

// quicConfig is how the box dials.
func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: dialTimeout,
		MaxIdleTimeout:       cloudwire.QUICIdleTimeout,
		KeepAlivePeriod:      cloudwire.QUICKeepAlive,
		EnableDatagrams:      true,
		// The service sends the box everything on the control stream, and
		// may open one bulk stream.
		MaxIncomingStreams:    -1,
		MaxIncomingUniStreams: 1,
	}
}

// dialQUIC opens the uplink over QUIC to addr, a host and port, on a UDP
// socket of its own (pc, or a new one when nil), and sends the target. With
// a ticket in tlsConf's session cache the target goes as early data, so the
// service's challenge comes back in its first flight; the box sends nothing
// else early. A service that rejects the early data gets the target again.
func dialQUIC(ctx context.Context, addr string, tlsConf *tls.Config, pc net.PacketConn) (*quicConn, error) {
	udp, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	if pc == nil {
		if pc, err = net.ListenUDP("udp", nil); err != nil {
			return nil, err
		}
	}
	tc := tlsConf.Clone()
	tc.NextProtos = []string{cloudwire.ALPNBox}
	tr := &quic.Transport{Conn: pc}
	dial := tr.Dial
	if tc.ClientSessionCache != nil {
		dial = tr.DialEarly
	}
	qc, err := dial(ctx, udp, tc, quicConfig())
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	fail := func(err error) (*quicConn, error) {
		_ = qc.CloseWithError(0, "")
		_ = tr.Close()
		return nil, err
	}
	// DialEarly comes back before the handshake only when it can send early
	// data, with a ticket that allows it.
	early := false
	select {
	case <-qc.HandshakeComplete():
	default:
		early = true
	}
	target := cloudwire.AppendMessage(nil, []byte(cloudwire.TargetBox))
	ctrl, err := qc.OpenStreamSync(ctx)
	if err == nil {
		_, err = ctrl.Write(target)
	}
	if err != nil {
		return fail(err)
	}
	select {
	case <-qc.HandshakeComplete():
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	if early && !qc.ConnectionState().Used0RTT {
		// The early data, if any went, was rejected: the connection quic-go
		// hands over takes the target again.
		if qc, err = qc.NextConnection(ctx); err != nil {
			return fail(err)
		}
		if ctrl, err = qc.OpenStreamSync(ctx); err == nil {
			_, err = ctrl.Write(target)
		}
		if err != nil {
			return fail(err)
		}
	}
	c := &quicConn{qc: qc, tr: tr, ctrl: ctrl, r: bufio.NewReader(ctrl), in: make(chan inbound),
		kick: make(chan struct{}, 1), limit: cloudwire.MaxTunFrame}
	go func() {
		<-qc.Context().Done()
		_ = tr.Close()
	}()
	return c, nil
}

func (c *quicConn) transport() string { return cloudwire.TransportQUIC }

func (c *quicConn) exporter() ([]byte, error) {
	cs := c.qc.ConnectionState().TLS
	return cs.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
}

func (c *quicConn) carries(messageType int) bool {
	switch messageType {
	case msgDatagram:
		d := c.qc.ConnectionState().SupportsDatagrams
		return d.Local && d.Remote
	case msgBulk:
		c.bulkMu.Lock()
		defer c.bulkMu.Unlock()
		return !c.noBulk
	}
	return messageType == websocket.BinaryMessage
}

func (c *quicConn) ReadMessage() (int, []byte, error) {
	c.start.Do(c.readers)
	for {
		c.mu.Lock()
		err, deadline := c.readErr, c.deadline
		c.mu.Unlock()
		if err != nil {
			return 0, nil, err
		}
		var expired <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return 0, nil, c.fail(errQUICTimeout)
			}
			timer = time.NewTimer(wait)
			expired = timer.C
		}
		select {
		case m := <-c.in:
			if timer != nil {
				timer.Stop()
			}
			return m.kind, m.msg, nil
		case <-expired:
			return 0, nil, c.fail(errQUICTimeout)
		case <-c.kick:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *quicConn) fail(err error) error {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	err = c.readErr
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return err
}

func (c *quicConn) readers() {
	ctx := c.qc.Context()
	deliver := func(kind int, msg []byte) bool {
		select {
		case c.in <- inbound{kind: kind, msg: msg}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	readStream := func(r *bufio.Reader, kind int) {
		for {
			c.mu.Lock()
			limit := c.limit
			c.mu.Unlock()
			msg, err := cloudwire.ReadMessage(r, int(limit))
			if err != nil {
				if kind == websocket.BinaryMessage || !errors.Is(err, io.EOF) {
					c.fail(c.readError(err))
				}
				return
			}
			if !deliver(kind, msg) {
				return
			}
		}
	}
	go readStream(c.r, websocket.BinaryMessage)
	go func() {
		for {
			d, err := c.qc.ReceiveDatagram(ctx)
			if err != nil || !deliver(msgDatagram, d) {
				return
			}
		}
	}()
	go func() {
		if str, err := c.qc.AcceptUniStream(ctx); err == nil {
			readStream(bufio.NewReader(str), msgBulk)
		}
	}()
	go func() {
		t := time.NewTicker(cloudwire.QUICKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.mu.Lock()
				ping := c.ping
				c.mu.Unlock()
				if ping != nil {
					_ = ping("")
				}
			}
		}
	}()
}

// readError is the service's close as a WebSocket close, so the link reads
// a close code the same over both transports.
func (c *quicConn) readError(err error) error {
	if ae, ok := errors.AsType[*quic.ApplicationError](err); ok {
		return &websocket.CloseError{Code: int(ae.ErrorCode), Text: ae.ErrorMessage}
	}
	if errors.Is(err, io.EOF) {
		return &websocket.CloseError{Code: websocket.CloseNormalClosure}
	}
	if errors.Is(err, cloudwire.ErrQUICMessage) {
		c.closeWith(cloudwire.CloseProtocol, "")
	}
	return err
}

func (c *quicConn) WriteMessage(messageType int, data []byte) error {
	switch messageType {
	case msgDatagram:
		if c.carries(msgDatagram) {
			err := c.qc.SendDatagram(data)
			if err == nil {
				return nil
			}
			if _, tooLarge := errors.AsType[*quic.DatagramTooLargeError](err); !tooLarge {
				return err
			}
		}
	case msgBulk:
		if sent, err := c.writeBulk(data); sent || err != nil {
			return err
		}
	case websocket.BinaryMessage:
	default:
		return errors.New("cloudlink: no such message type")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ctrl.SetWriteDeadline(c.writeDeadline()); err != nil {
		return err
	}
	_, err := c.ctrl.Write(cloudwire.AppendMessage(nil, data))
	return err
}

func (c *quicConn) writeDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDL
}

// writeBulk sends on the bulk stream, opening it the first time; false
// when the service grants no stream for it.
func (c *quicConn) writeBulk(data []byte) (bool, error) {
	c.bulkMu.Lock()
	defer c.bulkMu.Unlock()
	if c.noBulk {
		return false, nil
	}
	if c.bulk == nil {
		s, err := c.qc.OpenUniStream()
		if err != nil {
			c.noBulk = true
			return false, nil
		}
		c.bulk = s
	}
	if err := c.bulk.SetWriteDeadline(c.writeDeadline()); err != nil {
		return true, err
	}
	_, err := c.bulk.Write(cloudwire.AppendMessage(nil, data))
	return true, err
}

func (c *quicConn) WriteControl(messageType int, data []byte, _ time.Time) error {
	switch messageType {
	case websocket.CloseMessage:
		code, text := websocket.CloseNoStatusReceived, ""
		if len(data) >= 2 {
			code, text = int(binary.BigEndian.Uint16(data)), string(data[2:])
		}
		c.closeWith(code, text)
	case websocket.PingMessage, websocket.PongMessage:
		// QUIC's keepalive does this.
	}
	return nil
}

func (c *quicConn) closeWith(code int, text string) {
	c.closed.Do(func() { _ = c.qc.CloseWithError(quic.ApplicationErrorCode(code), text) })
}

func (c *quicConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return nil
}

func (c *quicConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDL = t
	c.mu.Unlock()
	return nil
}

func (c *quicConn) SetReadLimit(limit int64) {
	c.mu.Lock()
	c.limit = limit
	c.mu.Unlock()
}

func (c *quicConn) SetPingHandler(h func(appData string) error) {
	c.mu.Lock()
	c.ping = h
	c.mu.Unlock()
}

func (c *quicConn) Close() error {
	c.closeWith(websocket.CloseGoingAway, "")
	return nil
}

var errQUICTimeout error = quicTimeout{}

type quicTimeout struct{}

func (quicTimeout) Error() string   { return "cloudlink: read deadline passed" }
func (quicTimeout) Timeout() bool   { return true }
func (quicTimeout) Temporary() bool { return true }
