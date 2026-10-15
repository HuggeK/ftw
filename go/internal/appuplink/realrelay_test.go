package appuplink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
)

// The exchange end to end: this box, srcfl/ftw-webapp's relay, and a watch in
// Go posting over HTTP (ADR 0009, milestone 5). Skipped unless a relay with
// the exchange route is running:
//
//	PORT=8797 HOST=127.0.0.1 node relay/src/main.ts      # in srcfl/ftw-webapp
//	FTW_RELAY_URL=ws://127.0.0.1:8797 go test ./internal/appuplink -run RealRelay -v
//
// FTW_EV_SAVE_MS sets how long the charger's save takes, so the timing below
// can show what an applied command waits for. The relay allows 30 exchanges a
// minute per room, so the run stays under that.
func TestExchangeThroughARealRelay(t *testing.T) {
	endpoint := os.Getenv("FTW_RELAY_URL")
	if endpoint == "" {
		t.Skip("set FTW_RELAY_URL to a relay with the exchange route")
	}
	base := "http" + strings.TrimPrefix(endpoint, "ws")
	save, _ := strconv.Atoi(os.Getenv("FTW_EV_SAVE_MS"))

	charger := &fakeCharger{save: time.Duration(save) * time.Millisecond}
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	cmds := appproto.NewCmdLog()
	uplink, err := New(Options{
		Endpoint:        endpoint,
		Enroll:          enroll,
		Handler:         handlerWith(false, charger, cmds),
		ExchangeHandler: handlerWith(true, charger, cmds),
		Logger:          slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = uplink.Run(ctx) }()

	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phoneKey, err := appwire.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	phone, err := enroll.Authorise(phoneKey.Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	watchCode, _, err := enroll.MintWatchCode(phone.DeviceID, appenroll.WatchTTL)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}

	epoch := CurrentEpoch(time.Now().UnixMilli())
	post := func(body []byte) (int, []byte, time.Duration) {
		t.Helper()
		handle, err := Handle(enroll.RendezvousSecret(), epoch)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		resp, err := http.Post(fmt.Sprintf("%s/x/%d/%s", base, epoch, handle), "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		answer, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, answer, time.Since(started)
	}

	// X1. The box may still be joining, which the relay answers 404 (no box)
	// or 501 (a box with no exchange socket yet).
	message1, err := w.open(enroll.StaticKey().Public(), watchCode)
	if err != nil {
		t.Fatal(err)
	}
	var status int
	var message2 []byte
	for try := 0; try < 6; try++ {
		status, message2, _ = post(message1)
		if status == http.StatusConflict {
			if n, err := strconv.ParseInt(string(message2), 10, 64); err == nil {
				epoch = n
			}
			continue
		}
		if status != http.StatusNotFound && status != http.StatusNotImplemented {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status != http.StatusOK {
		t.Fatalf("X1 through the relay answered %d", status)
	}
	if len(message2) != appwire.Message2Overhead+1024 {
		t.Fatalf("message 2 is %d bytes", len(message2))
	}
	hello, _, err := w.opened(message2)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Role != apiauth.RoleWatch {
		t.Fatalf("hello_ok = %+v", hello)
	}

	exchange := func(msgType string, body any) ([]appwire.Frame, time.Duration) {
		t.Helper()
		request, err := w.request(msgType, body)
		if err != nil {
			t.Fatal(err)
		}
		status, answer, took := post(request)
		if status != http.StatusOK || len(answer) != exchangeAnswerFrames*exchangeRequestBytes {
			t.Fatalf("%s through the relay answered %d with %d bytes", msgType, status, len(answer))
		}
		frames, err := w.answer(answer)
		if err != nil {
			t.Fatal(err)
		}
		return frames, took
	}
	reopen := func() {
		t.Helper()
		message1, err := w.open(enroll.StaticKey().Public(), nil)
		if err != nil {
			t.Fatal(err)
		}
		status, message2, _ := post(message1)
		if status != http.StatusOK {
			t.Fatalf("reopening answered %d", status)
		}
		if _, _, err := w.opened(message2); err != nil {
			t.Fatal(err)
		}
	}

	// A charge, the same charge again, a refusal and a read: one session.
	frames, _ := exchange(appproto.MsgCmd, chargeNow("relay-1"))
	if got := types(frames); got[0] != appproto.MsgCmdAck || got[1] != appproto.MsgCmdResult {
		t.Fatalf("the charge answered %v", got)
	}
	frames, _ = exchange(appproto.MsgCmd, chargeNow("relay-1"))
	if got := types(frames); got[1] != appproto.MsgCmdResult || charger.count() != 1 {
		t.Fatalf("asking again answered %v after %d holds", got, charger.count())
	}
	frames, _ = exchange(appproto.MsgCmd, appproto.Cmd{CmdID: "relay-2", Op: appproto.OpSetMode,
		Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}})
	if res := decodeAs[appproto.CmdResult](t, frames[0]); res.Error == nil || res.Error.Code != appproto.ErrScopeDenied {
		t.Fatalf("site.mode.set from a watch answered %+v", res)
	}
	frames, _ = exchange(appproto.MsgNow, struct{}{})
	if frames[0].Envelope.T != appproto.MsgNow {
		t.Fatalf("now answered %v", types(frames))
	}

	// Timing (decision F): applied and refused, through the relay.
	var applied, refused []time.Duration
	for i := 0; i < 8; i++ {
		if i%2 == 0 {
			reopen()
		}
		_, took := exchange(appproto.MsgCmd, chargeNow(fmt.Sprintf("relay-timing-%d", i)))
		applied = append(applied, took)
		_, took = exchange(appproto.MsgCmd, appproto.Cmd{CmdID: fmt.Sprintf("relay-refused-%d", i), Op: appproto.OpSetMode,
			Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}})
		refused = append(refused, took)
	}
	t.Logf("through the relay, EV save %d ms: applied %s; refused %s", save, spread(applied), spread(refused))
}

// TestExchangeTimingAtTheBox times the same two answers without the relay.
// Skipped unless FTW_EXCHANGE_TIMING is set; it reports and asserts nothing.
func TestExchangeTimingAtTheBox(t *testing.T) {
	if os.Getenv("FTW_EXCHANGE_TIMING") == "" {
		t.Skip("set FTW_EXCHANGE_TIMING=1 to time answers at the box")
	}
	save, _ := strconv.Atoi(os.Getenv("FTW_EV_SAVE_MS"))
	r := newXRig(t)
	r.x.cmdFloor = ExchangeCmdFloor
	r.charger.save = time.Duration(save) * time.Millisecond
	var applied, refused []time.Duration
	var w *watchClient
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			// A second on the rig's clock per handshake keeps the run under
			// the sixty a minute the box allows.
			r.advance(time.Second)
			w, _ = r.addWatch(t)
		}
		started := time.Now()
		r.ask(t, w, appproto.MsgCmd, chargeNow(fmt.Sprintf("box-timing-%d", i)))
		applied = append(applied, time.Since(started))
		started = time.Now()
		r.ask(t, w, appproto.MsgCmd, appproto.Cmd{CmdID: fmt.Sprintf("box-refused-%d", i), Op: appproto.OpSetMode,
			Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}})
		refused = append(refused, time.Since(started))
	}
	t.Logf("at the box, EV save %d ms: applied %s; refused %s", save, spread(applied), spread(refused))
}

// spread is p50 and p95 of a sample.
func spread(d []time.Duration) string {
	s := slices.Clone(d)
	slices.Sort(s)
	at := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return fmt.Sprintf("p50 %s p95 %s (n=%d)", at(0.5).Round(10*time.Microsecond), at(0.95).Round(10*time.Microsecond), len(s))
}
