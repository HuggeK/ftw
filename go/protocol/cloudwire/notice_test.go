package cloudwire

import (
	"bytes"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// A notice body holds the header, a notice kind and, on agent.added, the
// client's name. Anything else, a reading above all, makes it no notice.
func TestReadNoticeTakesOnlyANotice(t *testing.T) {
	header := Header{Seq: 4, Boot: bytes.Repeat([]byte{0xb0}, BootBytes), UptimeMs: 9_000}
	body := func(n Notice, extra map[string]any) []byte {
		n.Header = header
		return noticeBody(t, n, extra)
	}
	for _, c := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"a kind", body(Notice{Kind: protocol.PushOwnerAdded}, nil), true},
		{"agent.added with its client", body(Notice{Kind: protocol.PushAgentAdded, Client: "Claude"}, nil), true},
		{"a name the consent page cut", body(Notice{Kind: protocol.PushAgentAdded, Client: strings.Repeat("ö", 64) + "…"}, nil), true},
		{"a client over the limit", body(Notice{Kind: protocol.PushAgentAdded, Client: strings.Repeat("c", NoticeClientMaxRunes+1)}, nil), false},
		{"a format character", body(Notice{Kind: protocol.PushAgentAdded, Client: "Claude\u202e"}, nil), false},
		{"a control character", body(Notice{Kind: protocol.PushAgentAdded, Client: "Claude\nPay here"}, nil), false},
		{"a short boot", func() []byte {
			b, _ := Marshal(Notice{Header: Header{Seq: 4, Boot: []byte{1, 2}}, Kind: protocol.PushOwnerAdded})
			return b
		}(), false},
		{"a client on another kind", body(Notice{Kind: protocol.PushAgentRequest, Client: "Claude"}, nil), false},
		{"a kind that is not a notice", body(Notice{Kind: protocol.PushChargingConnected}, nil), false},
		{"no kind", body(Notice{}, nil), false},
		{"a reading beside the kind", body(Notice{Kind: protocol.PushAgentRequest}, map[string]any{"fields": map[int]int64{1: 2400}}), false},
		{"a source id beside the kind", body(Notice{Kind: protocol.PushAgentRequest}, map[string]any{"src": "q5Lw0Zb1"}), false},
		{"not a map", []byte{0x01}, false},
	} {
		n, err := ReadNotice(c.body)
		if (err == nil) != c.valid {
			t.Errorf("%s: err %v, want valid %v", c.name, err, c.valid)
		}
		if c.valid && n.Seq != header.Seq {
			t.Errorf("%s: header lost: %+v", c.name, n.Header)
		}
	}
}
