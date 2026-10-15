package appuplink

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
)

// A watch reaches the box through the exchange and never through the stream,
// so it can never take a phone's place. Its key on the stream
// gets the silence a stranger gets.
func TestAWatchKeyGetsNoStream(t *testing.T) {
	r := newRig(t, 481234)

	code, _, err := r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	phone, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	grant, err := r.enroll.Authorise(phone.staticPublic(), code)
	if err != nil {
		t.Fatalf("pairing the phone: %v", err)
	}
	watchCode, _, err := r.enroll.MintWatchCode(grant.DeviceID, appenroll.WatchTTL)
	if err != nil {
		t.Fatalf("MintWatchCode: %v", err)
	}
	watch, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	if _, err := r.enroll.AuthoriseWatch(watch.staticPublic(), watchCode); err != nil {
		t.Fatalf("adding the watch: %v", err)
	}

	conn := r.dialApp(t, 481234)
	message1, err := watch.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatalf("message1: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message1); err != nil {
		t.Fatalf("sending message 1: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	for {
		kind, _, err := conn.ReadMessage()
		if err != nil {
			return // the deadline, which is the pass
		}
		if kind == websocket.BinaryMessage {
			t.Fatal("the box opened a stream for a watch")
		}
	}
}
