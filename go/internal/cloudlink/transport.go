package cloudlink

import (
	"crypto/tls"
	"errors"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// uplinkConn is what the uplink needs of a connection: gorilla/websocket's
// method set, which a WebSocket has as it is, plus the TLS exporter and the
// transport's name. quicConn is the other one (docs/protocol-cloud.md,
// "QUIC").
type uplinkConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetReadLimit(limit int64)
	SetPingHandler(h func(appData string) error)
	Close() error
}

// Message types beyond the WebSocket's, for what QUIC carries apart.
const (
	// msgBulk is a tun carrying a lane 1 frame, on the bulk stream.
	msgBulk = 0x100
	// msgDatagram is a QUIC DATAGRAM: never sent again, the newest wins.
	msgDatagram = 0x101
)

// carrier is what a connection that carries more than binary messages says.
type carrier interface {
	carries(messageType int) bool
	exporter() ([]byte, error)
	transport() string
}

// kindFor is the message type to write a message of the wanted type with:
// that one where conn carries it, a binary message anywhere else.
func kindFor(conn uplinkConn, want int) int {
	if c, ok := conn.(carrier); ok && c.carries(want) {
		return want
	}
	return websocket.BinaryMessage
}

// transportOf is the registry's name for how conn travels.
func transportOf(conn uplinkConn) string {
	if c, ok := conn.(carrier); ok {
		return c.transport()
	}
	return cloudwire.TransportWebSocket
}

// exporterValue is the TLS 1.3 exporter value both ends bind the handshake to.
func exporterValue(conn uplinkConn) ([]byte, error) {
	if c, ok := conn.(carrier); ok {
		return c.exporter()
	}
	ws, ok := conn.(*websocket.Conn)
	if !ok {
		return nil, errors.New("cloudlink: the uplink is not TLS")
	}
	tc, ok := ws.NetConn().(*tls.Conn)
	if !ok {
		return nil, errors.New("cloudlink: the uplink is not TLS")
	}
	cs := tc.ConnectionState()
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("cloudlink: the uplink is not TLS 1.3")
	}
	return cs.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
}
