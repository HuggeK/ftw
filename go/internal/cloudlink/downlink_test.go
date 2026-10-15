package cloudlink

import (
	"bytes"
	"slices"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

func frameOf(t testing.TB, typ string, body any) []byte {
	t.Helper()
	env, err := cloudwire.Seal(nil, "", typ, body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := env.Encode()
	frame, _ := cloudwire.EncodeFrame(raw)
	return frame
}

func TestDownlinkIsAClosedSet(t *testing.T) {
	for _, typ := range cloudwire.Downlink {
		if _, err := parseDownlink(websocket.BinaryMessage, frameOf(t, typ, map[string]int{})); err != nil {
			t.Errorf("%s refused: %v", typ, err)
		}
	}
	// What a service might try instead: the box's own messages, the app
	// protocol's command, and anything new.
	for _, typ := range []string{"cmd", "api.req", "hello", "live", "config", "update", "time", "ping"} {
		if _, err := parseDownlink(websocket.BinaryMessage, frameOf(t, typ, map[string]int{})); err == nil {
			t.Errorf("%s accepted", typ)
		}
	}
	if _, err := parseDownlink(websocket.TextMessage, frameOf(t, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})); err == nil {
		t.Error("a text message was accepted")
	}
	big := frameOf(t, cloudwire.MsgResync, cloudwire.Resync{What: []string{string(bytes.Repeat([]byte("x"), 5000))}})
	if _, err := parseDownlink(websocket.BinaryMessage, big); err == nil {
		t.Error("a frame over 4 KiB was accepted")
	}
	lane0 := frameOf(t, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})
	lane0[1] = 0
	if _, err := parseDownlink(websocket.BinaryMessage, lane0); err == nil {
		t.Error("a lane 0 frame was accepted")
	}
}

// Whatever the service sends, the box either refuses it or reads one of the
// five types, and never panics.
func FuzzDownlink(f *testing.F) {
	for _, typ := range append(slices.Clone(cloudwire.Downlink), "cmd") {
		f.Add(true, frameOf(f, typ, map[string]any{"hz": 1, "what": []string{"full"}}))
	}
	f.Add(false, []byte("ready"))
	f.Fuzz(func(t *testing.T, binary bool, msg []byte) {
		kind := websocket.TextMessage
		if binary {
			kind = websocket.BinaryMessage
		}
		env, err := parseDownlink(kind, msg)
		if err != nil {
			return
		}
		if !slices.Contains(cloudwire.Downlink, env.T) || len(msg) > cloudwire.MaxServiceFrame {
			t.Fatalf("accepted %q in %d bytes", env.T, len(msg))
		}
		// The bodies the sender reads must not panic either.
		_ = cloudwire.Unmarshal(env.B, &cloudwire.Watch{})
		_ = cloudwire.Unmarshal(env.B, &cloudwire.Resync{})
		_ = cloudwire.Unmarshal(env.B, &cloudwire.Purged{})
	})
}
