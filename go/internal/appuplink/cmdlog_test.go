package appuplink

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/control"
)

// countingModes is a mode controller that counts how often the box acted.
type countingModes struct {
	mu    sync.Mutex
	calls int
}

func (m *countingModes) SetMode(context.Context, control.Mode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return nil
}

func (m *countingModes) ObservedMode() (control.Mode, bool) {
	return control.ModeSelfConsumption, true
}

func (m *countingModes) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// handlerOver builds every session's handler over one box: one mode
// controller and one command log, as cmd/ftw wires them.
func handlerOver(modes appproto.ModeController, cmds *appproto.CmdLog) HandlerBuilder {
	return func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock:      appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:       stubSite{},
			Info:       stubInfo{},
			Modes:      modes,
			Plans:      stubPlans{},
			Codec:      codec{},
			Sender:     s,
			Caller:     caller,
			Grants:     grants,
			Commands:   cmds,
			SrcGrid:    "meter",
			SrcPV:      "meter",
			SrcBattery: "meter",
			Logger:     slog.New(slog.DiscardHandler),
		})
	}
}

// openSession runs one handshake and hello for a phone and returns its socket.
// code is the pairing code on a first run and nil after it.
func openSession(t *testing.T, r *rig, app *appClient, code []byte) *websocket.Conn {
	t.Helper()

	conn := r.dialApp(t, 481234)
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
		App:   appproto.AppInfo{Build: "test", UA: "pwa"},
	})
	if got := receive(t, conn, app); got.Envelope.T != appproto.MsgHelloOK {
		t.Fatalf("first reply is %q, want %q", got.Envelope.T, appproto.MsgHelloOK)
	}
	return conn
}

// sendCmdAndRead sends one command and returns its ack and result. An applied
// command also pushes a plan, which is read off the socket here so it cannot
// reach the next session's socket.
func sendCmdAndRead(t *testing.T, conn *websocket.Conn, app *appClient, cmd appproto.Cmd) (appproto.CmdAck, appproto.CmdResult) {
	t.Helper()

	send(t, conn, app, appproto.MsgCmd, 512, cmd)

	var ack appproto.CmdAck
	if f := receive(t, conn, app); f.Envelope.T != appproto.MsgCmdAck {
		t.Fatalf("first answer is %q, want %q", f.Envelope.T, appproto.MsgCmdAck)
	} else if err := appproto.Unmarshal(f.Envelope.B, &ack); err != nil {
		t.Fatalf("decoding the ack: %v", err)
	}

	var res appproto.CmdResult
	if f := receive(t, conn, app); f.Envelope.T != appproto.MsgCmdResult {
		t.Fatalf("second answer is %q, want %q", f.Envelope.T, appproto.MsgCmdResult)
	} else if err := appproto.Unmarshal(f.Envelope.B, &res); err != nil {
		t.Fatalf("decoding the result: %v", err)
	}
	return ack, res
}

func drainPlan(t *testing.T, conn *websocket.Conn, app *appClient) {
	t.Helper()
	if f := receive(t, conn, app); f.Envelope.T != appproto.MsgPlan {
		t.Fatalf("an applied command was followed by %q, want %q", f.Envelope.T, appproto.MsgPlan)
	}
}

// A phone that loses its socket mid-command retries in a new session. The box
// must answer the retry with what happened the first time, not act again.
// Before the box kept the command log, each session had its own, and this
// retry set the mode twice.
func TestARetryInANewSessionActsOnce(t *testing.T) {
	modes := &countingModes{}
	r := newRigWith(t, 481234, handlerOver(modes, appproto.NewCmdLog()))

	code, _, err := r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	phone, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}

	cmd := appproto.Cmd{
		CmdID:           "0192f2a0-7c1e-7000-8000-0123456789ab",
		Op:              appproto.OpSetMode,
		Args:            map[string]any{"mode": string(control.ModeSelfConsumption)},
		NotValidAfterMs: 10 * 60 * 1000,
		Expect:          appproto.Expect{Rev: 1},
	}

	first := openSession(t, r, phone, code)
	firstAck, firstRes := sendCmdAndRead(t, first, phone, cmd)
	if firstRes.State != appproto.CmdApplied {
		t.Fatalf("first result is %q, want applied", firstRes.State)
	}
	drainPlan(t, first, phone)
	_ = first.Close()

	// The same phone, a new socket and a new handshake: the app keeps its
	// static key and nothing else across a reconnect.
	again := &appClient{static: phone.static}
	second := openSession(t, r, again, nil)
	retryAck, retryRes := sendCmdAndRead(t, second, again, cmd)
	_ = second.Close()

	if n := modes.count(); n != 1 {
		t.Fatalf("the box set the mode %d times for one command id", n)
	}
	if retryAck.LeaseID != firstAck.LeaseID {
		t.Fatalf("the retry got lease %q, the first send %q", retryAck.LeaseID, firstAck.LeaseID)
	}
	if retryRes.State != firstRes.State {
		t.Fatalf("the retry got %q, the first send %q", retryRes.State, firstRes.State)
	}

	// Another phone that happens to mint the same id means its own intent.
	code, _, err = r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	other, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	third := openSession(t, r, other, code)
	otherAck, otherRes := sendCmdAndRead(t, third, other, cmd)
	drainPlan(t, third, other)

	if n := modes.count(); n != 2 {
		t.Fatalf("the box set the mode %d times; another device's id must count as new", n)
	}
	if otherRes.State != appproto.CmdApplied {
		t.Fatalf("the other device's result is %q, want applied", otherRes.State)
	}
	if otherAck.LeaseID == firstAck.LeaseID {
		t.Fatal("another device's command replayed the first device's lease")
	}
}
