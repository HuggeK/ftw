package cloudwire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/srcfl/ftw/go/protocol"
)

// QUIC: the uplink and the app session over QUIC on UDP 443, beside the
// WebSocket. docs/protocol-cloud.md, "QUIC", is the normative text. The
// messages are the WebSocket's; only the framing is new.

// ALPN names, one per connection.
const (
	ALPNBox = "ftw-cloud-box/1"
	ALPNApp = "ftw-cloud-app/1"
)

// The target is the first message on the control stream: the path and query
// the WebSocket would name.
const (
	TargetBox = "/v1/box"
	// AppPath is the app session's path; AppTarget adds the home.
	AppPath = "/v1/app"
	// MaxTarget bounds the target message.
	MaxTarget = 256
)

// AppTarget is the target of an app session with home.
func AppTarget(home string) string { return AppPath + "?" + AppHomeParam + "=" + home }

// Timing, shared by every client and the service.
const (
	// QUICKeepAlive is how long a client stays silent before it sends a
	// QUIC PING, well inside the 30 s after which many NATs forget a UDP
	// mapping.
	QUICKeepAlive = 15 * time.Second
	// QUICIdleTimeout closes a connection silent this long, as the
	// WebSocket's read deadline does.
	QUICIdleTimeout = 45 * time.Second
	// RaceDelay is how long QUIC leads before a client also dials the
	// WebSocket.
	RaceDelay = 250 * time.Millisecond
	// EarlyDataFor is how long a session ticket allows 0-RTT data.
	EarlyDataFor = 24 * time.Hour
	// QUICProvenAfter is how long a QUIC session must last past its first
	// session message before the path dropping it stops counting as QUIC
	// failing on that network.
	QUICProvenAfter = 60 * time.Second
)

// The transports under a carrier, the registry's cloud_transports, as
// generated from it.
const (
	TransportQUIC      = protocol.CloudTransportQuic
	TransportWebSocket = protocol.CloudTransportWebsocket
)

// Transports lists them in the registry's order.
var Transports = protocol.CloudTransports

// ErrQUICMessage is a message the QUIC framing refuses: a length over the
// limit, or one cut short.
var ErrQUICMessage = errors.New("cloudwire: not a QUIC message")

// maxVarint is the largest QUIC variable-length integer.
const maxVarint = 1<<62 - 1

// AppendVarint appends v as a QUIC variable-length integer (RFC 9000 §16):
// the two high bits of the first byte give the length, 1, 2, 4 or 8 bytes.
func AppendVarint(dst []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(dst, byte(v))
	case v < 1<<14:
		return append(dst, 0x40|byte(v>>8), byte(v))
	case v < 1<<30:
		return append(dst, 0x80|byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	case v <= maxVarint:
		return append(dst, 0xc0|byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	panic("cloudwire: varint out of range")
}

// ReadVarint reads one QUIC variable-length integer.
func ReadVarint(r io.ByteReader) (uint64, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	n := 1 << (b >> 6)
	v := uint64(b & 0x3f)
	for i := 1; i < n; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// AppendMessage appends one message as it goes on a stream: its length as
// a varint, then its bytes.
func AppendMessage(dst, msg []byte) []byte {
	return append(AppendVarint(dst, uint64(len(msg))), msg...)
}

// ReadMessage reads one message from a stream. A length over max is
// ErrQUICMessage and nothing of the message is read. io.EOF means the
// stream ended cleanly between messages.
func ReadMessage(r *bufio.Reader, max int) ([]byte, error) {
	n, err := ReadVarint(r)
	if err != nil {
		return nil, err
	}
	if n > uint64(max) {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrQUICMessage, n, max)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return msg, nil
}

// Datagram reports whether an envelope may go as a QUIC DATAGRAM: an hb, or
// a live without sources. Both are newest-wins: a lost one shows as a gap in
// the freshness rules, never as a value that looks live. A live that carries
// its source table must arrive, and so must everything else.
func Datagram(env Envelope) bool {
	switch env.T {
	case MsgHB:
		return true
	case MsgLive:
		var keys map[string]cbor.RawMessage
		if decMode.Unmarshal(env.B, &keys) != nil {
			return false
		}
		_, table := keys["sources"]
		return !table
	}
	return false
}
