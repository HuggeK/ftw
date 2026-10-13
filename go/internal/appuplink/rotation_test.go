package appuplink

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// roomRelay is the part of srcfl/ftw-webapp relay/src/server.ts that
// fakeRelay leaves out: a room per handle, an exact epoch on every join, and a
// rotation that closes each old room with 4410 on its own turn.
//
// The test is the relay's clock. turn() is the heartbeat that notices the
// hour; until then the relay still refuses the new epoch. rotate() is one
// room's turn in the five-minute spread; until then the old room stays open.
type roomRelay struct {
	server *httptest.Server

	mu    sync.Mutex
	epoch int64
	rooms map[string]*relayRoom
	// refused counts joins answered with 4409.
	refused int
}

type relayRoom struct {
	epoch   int64
	uplink  *relayPeer
	streams []*relayPeer
}

type relayPeer struct {
	conn *websocket.Conn
	// writeMu because gorilla allows one writer at a time, and the relay
	// writes to a peer from the other peers' read loops.
	writeMu sync.Mutex
	linked  bool
}

func (p *relayPeer) write(kind int, message []byte) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.WriteMessage(kind, message)
}

func (p *relayPeer) close(code int, reason string) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = p.conn.Close()
}

func newRoomRelay(t *testing.T, epoch int64) *roomRelay {
	t.Helper()
	r := &roomRelay{epoch: epoch, rooms: map[string]*relayRoom{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.server.Close)
	return r
}

func (r *roomRelay) url() string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http")
}

// turn is the relay's heartbeat noticing that the hour has changed.
func (r *roomRelay) turn(epoch int64) {
	r.mu.Lock()
	r.epoch = epoch
	r.mu.Unlock()
}

// rotate is one old room's turn in the spread: everyone in it hears 4410 and
// the new epoch, and the room is gone.
func (r *roomRelay) rotate(handle string) {
	r.mu.Lock()
	room := r.rooms[handle]
	if room == nil || room.epoch >= r.epoch {
		r.mu.Unlock()
		return
	}
	delete(r.rooms, handle)
	peers := append([]*relayPeer(nil), room.streams...)
	if room.uplink != nil {
		peers = append(peers, room.uplink)
	}
	epoch := r.epoch
	r.mu.Unlock()

	for _, p := range peers {
		p.close(CloseRotated, strconv.FormatInt(epoch, 10))
	}
}

func (r *roomRelay) refusals() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

func (r *roomRelay) hasUplink(handle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[handle]
	return room != nil && room.uplink != nil
}

func (r *roomRelay) handle(w http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.URL.Path, "/deadman") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "r" {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	handle, role := parts[2], parts[3]

	conn, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
	if err != nil {
		return
	}
	peer := &relayPeer{conn: conn}

	r.mu.Lock()
	if epoch != r.epoch {
		// Exact, as server.ts is: the old epoch is refused the moment the
		// relay has turned, even while old rooms are still open.
		want := r.epoch
		r.refused++
		r.mu.Unlock()
		peer.close(CloseEpoch, strconv.FormatInt(want, 10))
		return
	}
	room := r.rooms[handle]
	if room == nil {
		room = &relayRoom{epoch: epoch}
		r.rooms[handle] = room
	}
	if role == "box" {
		if room.uplink != nil {
			r.mu.Unlock()
			peer.close(CloseBusy, "uplink")
			return
		}
		room.uplink = peer
	} else {
		room.streams = append(room.streams, peer)
	}
	r.sync(room)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if room.uplink == peer {
			room.uplink = nil
		}
		for i, s := range room.streams {
			if s == peer {
				room.streams = append(room.streams[:i], room.streams[i+1:]...)
				break
			}
		}
		if room.uplink == nil && len(room.streams) == 0 {
			if r.rooms[handle] == room {
				delete(r.rooms, handle)
			}
		} else {
			r.sync(room)
		}
		r.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			if role == "box" && strings.HasPrefix(string(message), CtrlDeadman+" ") {
				continue
			}
			return
		}
		r.mu.Lock()
		var to []*relayPeer
		if room.uplink == peer {
			to = append(to, room.streams...)
		} else if room.uplink != nil {
			to = append(to, room.uplink)
		}
		r.mu.Unlock()
		for _, p := range to {
			p.write(websocket.BinaryMessage, message)
		}
	}
}

// sync tells each side whether the other is there, as server.ts does. Called
// with r.mu held.
func (r *roomRelay) sync(room *relayRoom) {
	notify := func(p *relayPeer, present bool) {
		if p.linked == present {
			return
		}
		p.linked = present
		word := CtrlGone
		if present {
			word = CtrlReady
		}
		go p.write(websocket.TextMessage, []byte(word))
	}
	if room.uplink != nil {
		notify(room.uplink, len(room.streams) > 0)
	}
	for _, s := range room.streams {
		notify(s, room.uplink != nil)
	}
}

// manualClock is the box's wall clock, moved by the test.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type rotationRig struct {
	relay  *roomRelay
	clock  *manualClock
	enroll *appenroll.Identity
	uplink *Uplink
}

// newRotationRig starts a box whose clock reads one minute before the end of
// epoch, on a relay that is on epoch.
func newRotationRig(t *testing.T, epoch int64) *rotationRig {
	t.Helper()
	relay := newRoomRelay(t, epoch)
	clock := &manualClock{t: time.UnixMilli((epoch+1)*EpochMs - 60_000)}

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	u, err := New(Options{
		Endpoint: relay.url(),
		Enroll:   enroll,
		Handler:  newHandler,
		Logger:   slog.New(slog.DiscardHandler),
		Now:      clock.Now,
		Random:   func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u.turnCheck = 5 * time.Millisecond
	u.turnRetry = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	r := &rotationRig{relay: relay, clock: clock, enroll: enroll, uplink: u}
	r.waitForUplink(t, epoch)
	return r
}

func (r *rotationRig) handle(t *testing.T, epoch int64) string {
	t.Helper()
	h, err := Handle(r.enroll.RendezvousSecret(), epoch)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return h
}

func (r *rotationRig) waitForUplink(t *testing.T, epoch int64) {
	t.Helper()
	handle := r.handle(t, epoch)
	deadline := time.Now().Add(5 * time.Second)
	for !r.relay.hasUplink(handle) {
		if time.Now().After(deadline) {
			t.Fatalf("the box never joined epoch %d", epoch)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openSession joins epoch's room as a phone and runs a handshake and a hello.
// It reports whether the box answered within wait.
func (r *rotationRig) openSession(t *testing.T, epoch int64, wait time.Duration) (*websocket.Conn, *appClient, bool) {
	t.Helper()
	code, _, err := r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(
		r.relay.url()+"/r/"+strconv.FormatInt(epoch, 10)+"/"+r.handle(t, epoch)+"/app", nil)
	if err != nil {
		t.Fatalf("dialling as an app: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The app's own rule: wait for "ready", because a frame written into an
	// empty room is dropped.
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return conn, nil, false
		}
		if kind == websocket.TextMessage && string(message) == CtrlReady {
			break
		}
	}

	app, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	message1, err := app.message1(r.enroll.StaticKey().Public(), code)
	if err != nil {
		t.Fatalf("message1: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message1); err != nil {
		t.Fatalf("sending message 1: %v", err)
	}
	if err := app.readMessage2(waitForBinary(t, conn)); err != nil {
		t.Fatalf("readMessage2: %v", err)
	}
	send(t, conn, app, appproto.MsgHello, 512, appproto.Hello{
		Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "test", UA: "ios"},
	})
	if got := receive(t, conn, app); got.Envelope.T != appproto.MsgHelloOK {
		t.Fatalf("first reply is %q, want %q", got.Envelope.T, appproto.MsgHelloOK)
	}
	return conn, app, true
}

// A phone that opens the app just after the hour derives the new hour's
// handle from its clock. The relay has turned and refuses the old epoch, but
// it closes the box's old room only on that room's turn in its five-minute
// spread. Seen on 13 Oct 2026: the phone waited from 09:00 until the relay
// rotated the box at 09:03:18. The box must already be on the new handle.
func TestAPhoneOpenedAfterTheHourFindsTheBox(t *testing.T) {
	const epoch = 497742
	r := newRotationRig(t, epoch)

	// 09:00:30 on both clocks. The old room's turn has not come.
	r.clock.Set(time.UnixMilli((epoch+1)*EpochMs + 30_000))
	r.relay.turn(epoch + 1)

	if _, _, ok := r.openSession(t, epoch+1, 3*time.Second); !ok {
		t.Fatal("a phone on the new hour's handle found no box until the relay rotated the old room")
	}
}

// A phone already talking to the box must not lose it at the hour. The box
// keeps the old handle until the relay closes that room, and the phone moves
// when the relay tells it to.
func TestAPhoneConnectedAcrossTheHourKeepsTheBox(t *testing.T) {
	const epoch = 497742
	r := newRotationRig(t, epoch)
	conn, app, ok := r.openSession(t, epoch, 3*time.Second)
	if !ok {
		t.Fatal("no session before the hour")
	}

	r.clock.Set(time.UnixMilli((epoch+1)*EpochMs + 30_000))
	r.relay.turn(epoch + 1)
	r.waitForUplink(t, epoch+1)

	// Still the old room, still answered.
	send(t, conn, app, appproto.MsgSub, 512, appproto.Sub{Bucket: 512})
	if got := receive(t, conn, app); got.Envelope.T != appproto.MsgSnap {
		t.Fatalf("after the hour the old session got %q, want %q", got.Envelope.T, appproto.MsgSnap)
	}

	// The room's turn. The phone follows to the new handle and the box is
	// there; the box did not have to move.
	r.relay.rotate(r.handle(t, epoch))
	if _, _, ok := r.openSession(t, epoch+1, 3*time.Second); !ok {
		t.Fatal("after the old room rotated the phone found no box on the new handle")
	}
	if !r.relay.hasUplink(r.handle(t, epoch+1)) {
		t.Fatal("the box left the new handle when the old room rotated")
	}
}

// The relay reads its clock on a fifteen-second heartbeat, so just after the
// hour it may still refuse the new epoch. That is "not yet", not a correction
// to an hour ago: the box keeps the old room and tries again.
func TestTheBoxWaitsForARelayThatHasNotTurned(t *testing.T) {
	const epoch = 497742
	r := newRotationRig(t, epoch)

	r.clock.Set(time.UnixMilli((epoch+1)*EpochMs + 1_000))
	deadline := time.Now().Add(5 * time.Second)
	for r.relay.refusals() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the box never tried the new hour's handle")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !r.relay.hasUplink(r.handle(t, epoch)) {
		t.Fatal("a refused try took the box out of the old room")
	}

	r.relay.turn(epoch + 1)
	r.waitForUplink(t, epoch+1)
	if !r.relay.hasUplink(r.handle(t, epoch)) {
		t.Fatal("the box left the old room before the relay closed it")
	}
}
