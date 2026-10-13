package cloudlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

func (k *memKV) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]string{}
	}
	k.m[key] = value
	return nil
}

// The fake house uses these names, so a test can search every frame for them.
const (
	meterName   = "p1meter-in-the-hall"
	chargerName = "easee-garage"
	lpName      = "garage-loadpoint"
)

type fakeSite struct {
	mu        sync.Mutex
	grid      int64
	state     protocol.SourceState
	lastOk    int64
	liveCalls int
	lpCalls   int
}

func (s *fakeSite) setGrid(w int64) {
	s.mu.Lock()
	s.grid = w
	s.mu.Unlock()
}

func (s *fakeSite) Live() Live {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveCalls++
	return Live{
		Fields:       map[string]int64{"1": 0, "2": s.grid, "6": 900, "10": 2100},
		FieldSources: map[string][]string{"2": {meterName}, "6": {meterName, chargerName}, "10": {chargerName}},
		Sources: map[string]Source{
			meterName:   {Kind: "driver", State: s.state, LastOkUptimeMs: s.lastOk, StaleAfterMs: 4_000},
			chargerName: {Kind: "driver", State: protocol.SourceLive, LastOkUptimeMs: s.lastOk, StaleAfterMs: 30_000},
		},
		DispatchBlockedBy: nil,
	}
}

func (s *fakeSite) Loadpoints() (cloudwire.LP, bool) {
	s.mu.Lock()
	s.lpCalls++
	s.mu.Unlock()
	soc := int64(430)
	return cloudwire.LP{ControlRev: 9, Loadpoints: []cloudwire.Loadpoint{{
		ID: lpName, Src: chargerName, Plugged: true, SocPermille: &soc, SocSource: "inferred", PowerW: 2100,
	}}, Feedback: []cloudwire.Feedback{{Kind: "ev", Src: chargerName, Status: "following"}}}, true
}

func (s *fakeSite) Prices() (cloudwire.Prices, bool) {
	p := int64(42)
	return cloudwire.Prices{Zone: "SE3", Currency: "SEK", T0Ms: 1_791_820_800_000, SlotMs: 900_000,
		SpotMinor: []*int64{&p}, TotalMinor: []*int64{&p}}, true
}

type fakeRoster struct{}

func (fakeRoster) Devices() []RosterDevice {
	return []RosterDevice{{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{5}, 32), Role: "owner", Epoch: 1,
		Scopes: []string{protocol.ScopeLiveRead, protocol.ScopeDispatchWrite, protocol.ScopeEventsRead}}}
}

// fakeService is ftwcloudd's half of the handshake, enough to test the box.
type fakeService struct {
	t   *testing.T
	key *ecdsa.PrivateKey
	srv *httptest.Server

	// Knobs, set before the box connects.
	badEKM      bool
	wrongKey    bool
	purge       bool
	afterHello  func(conn *websocket.Conn, home string)
	rosterSeqOK uint64

	mu    sync.Mutex
	conns int
	hello []byte
	got   []cloudwire.Envelope
	raw   [][]byte
	ch    chan cloudwire.Envelope
	home  string
	pub   *ecdsa.PublicKey
}

func newFakeService(t *testing.T) *fakeService {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &fakeService{t: t, key: key, ch: make(chan cloudwire.Envelope, 256)}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeService) endpoint() string {
	return "wss://" + s.srv.Listener.Addr().String() + "/v1/box"
}

func (s *fakeService) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	return pool
}

func (s *fakeService) write(conn *websocket.Conn, key *ecdsa.PrivateKey, home, t string, body any) {
	env, err := cloudwire.Seal(key, home, t, body)
	if err != nil {
		s.t.Error(err)
		return
	}
	raw, _ := env.Encode()
	frame, _ := cloudwire.EncodeFrame(raw)
	_ = conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (s *fakeService) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	ekm, _ := r.TLS.ExportKeyingMaterial(cloudwire.EKMLabel, nil, cloudwire.EKMBytes)
	nonce := bytes.Repeat([]byte{7}, cloudwire.NonceBytes)
	s.write(conn, nil, "", cloudwire.MsgChallenge, cloudwire.Challenge{V: 1, Nonce: nonce})

	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	payload, _ := cloudwire.DecodeFrame(msg)
	env, err := cloudwire.DecodeEnvelope(payload)
	var hello cloudwire.Hello
	if err != nil || env.T != cloudwire.MsgHello || cloudwire.Unmarshal(env.B, &hello) != nil {
		s.t.Errorf("first box message was not a hello: %v", err)
		return
	}
	pub, err := cloudwire.ParsePublicKey(hello.PK)
	home := cloudwire.HomeID(hello.PK)
	if err != nil || cloudwire.Verify(pub, home, env) != nil {
		s.t.Errorf("hello does not verify: %v", err)
		return
	}
	if !bytes.Equal(hello.Nonce, nonce) || !bytes.Equal(hello.EKM, ekm) {
		s.t.Errorf("hello is not bound to this challenge and TLS session")
		return
	}
	s.mu.Lock()
	s.home, s.pub, s.hello = home, pub, msg
	s.mu.Unlock()

	okEKM := ekm
	if s.badEKM {
		okEKM = bytes.Repeat([]byte{1}, cloudwire.EKMBytes)
	}
	signer := s.key
	if s.wrongKey {
		signer, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	s.write(conn, signer, home, cloudwire.MsgHelloOK, cloudwire.HelloOK{
		BoxNonce: hello.BoxNonce, EKM: okEKM, Have: map[string]string{}, RosterSeq: s.rosterSeqOK})
	if s.afterHello != nil {
		go s.afterHello(conn, home)
	}

	for {
		kind, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			s.t.Errorf("the box sent a text message")
			return
		}
		payload, err := cloudwire.DecodeFrame(msg)
		if err != nil {
			s.t.Errorf("box frame: %v", err)
			return
		}
		env, err := cloudwire.DecodeEnvelope(payload)
		if err != nil || cloudwire.Verify(pub, home, env) != nil {
			s.t.Errorf("box envelope %q does not verify: %v", env.T, err)
			return
		}
		s.mu.Lock()
		s.got = append(s.got, env)
		s.raw = append(s.raw, msg)
		s.mu.Unlock()
		s.ch <- env
		if env.T == cloudwire.MsgBye && s.purge {
			var bye cloudwire.Bye
			_ = cloudwire.Unmarshal(env.B, &bye)
			var grantID []byte
			for _, e := range s.received() {
				var r cloudwire.Roster
				if e.T == cloudwire.MsgRoster && cloudwire.Unmarshal(e.B, &r) == nil {
					grantID = r.Grant.ID
				}
			}
			if bye.Purge {
				s.write(conn, s.key, home, cloudwire.MsgPurged, cloudwire.Purged{
					AtMs: 1_791_821_000_000, What: cloudwire.PurgedHome, GrantID: grantID})
			}
		}
	}
}

func (s *fakeService) received() []cloudwire.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cloudwire.Envelope(nil), s.got...)
}

// waitFor returns the next envelope of type t, failing after d.
func (s *fakeService) waitFor(t string, d time.Duration) cloudwire.Envelope {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case env := <-s.ch:
			if env.T == t {
				return env
			}
		case <-deadline:
			s.t.Fatalf("no %s within %s", t, d)
			return cloudwire.Envelope{}
		}
	}
}

type harness struct {
	link    *Link
	svc     *fakeService
	site    *fakeSite
	kv      *memKV
	keyPath string
	cancel  context.CancelFunc
	done    chan error
	pushed  chan string
}

func newHarness(t *testing.T, svc *fakeService) *harness {
	t.Helper()
	h := &harness{svc: svc, site: &fakeSite{grid: 1200, state: protocol.SourceLive}, kv: &memKV{},
		keyPath: filepath.Join(t.TempDir(), "cloud.key"), pushed: make(chan string, 8)}
	link, err := New(Options{
		Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: h.keyPath, Build: "test",
		Started: time.Now().Add(-time.Minute), ClockSynced: func() bool { return true },
		Notify:   func(kind string) { h.pushed <- kind },
		Endpoint: svc.endpoint(), ServiceKeys: []*ecdsa.PublicKey{&svc.key.PublicKey},
		RootCAs: svc.roots(), Tick: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.link = link
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- link.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

func (h *harness) grant(t *testing.T, scopes ...string) {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{protocol.ScopeLiveRead}
	}
	if err := h.link.Grant(GrantRequest{Scopes: scopes, Policy: "2026-10", By: "test"}); err != nil {
		t.Fatal(err)
	}
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

func TestOffUntilAnOwnerGrants(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	time.Sleep(300 * time.Millisecond)
	svc.mu.Lock()
	conns := svc.conns
	svc.mu.Unlock()
	if conns != 0 {
		t.Fatalf("the box dialled FTW Cloud %d times with no grant", conns)
	}
	if st := h.link.Status(); st.State != "off" || st.Home != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key exists before any grant")
	}
}

func TestGrantStreamsSignedStateWithoutNames(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	if kind := <-h.pushed; kind != protocol.PushCloudGranted {
		t.Fatalf("owners were told %q", kind)
	}

	roster := svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	live := svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	svc.waitFor(cloudwire.MsgLP, 5*time.Second)
	svc.waitFor(cloudwire.MsgPrices, 5*time.Second)

	var r cloudwire.Roster
	if err := cloudwire.Unmarshal(roster.B, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Devices) != 1 || strings.Join(r.Devices[0].Scopes, ",") != "ftw.live.read,ftw.events.read" {
		t.Fatalf("roster devices %+v: write scopes must not leave the house", r.Devices)
	}
	var l cloudwire.Live
	if err := cloudwire.Unmarshal(live.B, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Sources) != 2 || l.SrcRev != 1 || l.Fields["2"] != 1200 || len(l.Ages) != 2 {
		t.Fatalf("first live %+v", l)
	}

	// An hb follows within H (S = 4 s, so H = 2 s) when nothing changes.
	svc.waitFor(cloudwire.MsgHB, 4*time.Second)

	for _, raw := range svc.raw {
		for _, name := range []string{meterName, chargerName, lpName} {
			if bytes.Contains(raw, []byte(name)) {
				t.Fatalf("%q left the house in %x", name, raw)
			}
		}
	}
	st := h.link.Status()
	if st.State != "on" || !st.Connected || st.Home != svc.home || st.BytesLast24h == 0 {
		t.Fatalf("status %+v", st)
	}
}

// What the service holds is exactly what the preview shows.
func TestPreviewIsTheBytesThatLeft(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	svc.waitFor(cloudwire.MsgPrices, 5*time.Second)
	svc.waitFor(cloudwire.MsgHB, 4*time.Second)
	h.cancel() // freeze: nothing more is sent
	<-h.done
	h.done <- nil

	latest := map[string][]byte{}
	svc.mu.Lock()
	latest[cloudwire.MsgHello] = svc.hello
	for i, env := range svc.got {
		latest[env.T] = svc.raw[i]
	}
	svc.mu.Unlock()

	p := h.link.Preview()
	if p.Sample || len(p.Frames) == 0 {
		t.Fatalf("preview %+v", p)
	}
	for _, f := range p.Frames {
		if !bytes.Equal(f.Frame, latest[f.T]) {
			t.Fatalf("%s: preview and service differ\npreview %x\nservice %x", f.T, f.Frame, latest[f.T])
		}
		if f.Body == nil {
			t.Fatalf("%s: no decoded body", f.T)
		}
	}
}

func TestPreviewBeforeAGrantIsAnUnsignedSample(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	p := h.link.Preview()
	if !p.Sample || p.State != "off" || len(p.Frames) != 3 {
		t.Fatalf("preview %+v", p)
	}
	for _, f := range p.Frames {
		payload, _ := cloudwire.DecodeFrame(f.Frame)
		env, err := cloudwire.DecodeEnvelope(payload)
		if err != nil || len(env.Sig) != 0 {
			t.Fatalf("%s: sample is signed or broken: %v", f.T, err)
		}
		if bytes.Contains(f.Frame, []byte(meterName)) {
			t.Fatal("a driver name is in the sample")
		}
	}
}

func TestAServiceThatFailsTheBindingGetsOnlyHello(t *testing.T) {
	for name, tweak := range map[string]func(*fakeService){
		"another TLS session": func(s *fakeService) { s.badEKM = true },
		"another key":         func(s *fakeService) { s.wrongKey = true },
	} {
		t.Run(name, func(t *testing.T) {
			svc := newFakeService(t)
			tweak(svc)
			h := newHarness(t, svc)
			h.grant(t)
			time.Sleep(time.Second)
			if got := svc.received(); len(got) != 0 {
				t.Fatalf("the box sent %d messages after a hello_ok that failed", len(got))
			}
			if h.link.Status().Connected {
				t.Fatal("the link reports connected")
			}
		})
	}
}

func TestUnknownDownlinkClosesTheUplink(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn *websocket.Conn, home string) {
		time.Sleep(200 * time.Millisecond)
		svc.write(conn, nil, home, "cmd", map[string]any{"op": "site.mode.set"})
	}
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatal("the box kept the uplink after a message the service may not send")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchRaisesTheCadence(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn *websocket.Conn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})
	}
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	h.site.setGrid(1500)
	start := time.Now()
	svc.waitFor(cloudwire.MsgLive, 3*time.Second)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("a watched change took %s", d)
	}
}

func TestATableChangeGoesAtOnceWithANewRevision(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	h.site.mu.Lock()
	h.site.state = protocol.SourceLagging
	h.site.mu.Unlock()
	// Within the one-second look at the table, well inside the 5 s period.
	env := svc.waitFor(cloudwire.MsgLive, 2*time.Second)
	var l cloudwire.Live
	_ = cloudwire.Unmarshal(env.B, &l)
	if l.SrcRev != 2 || len(l.Sources) != 2 {
		t.Fatalf("table change sent as %+v", l)
	}
}

func TestRevokeDeletesTheKeyOnlyAfterTheReceipt(t *testing.T) {
	svc := newFakeService(t)
	svc.purge = true
	h := newHarness(t, svc)
	h.grant(t)
	<-h.pushed
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)

	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	if kind := <-h.pushed; kind != protocol.PushCloudRevoked {
		t.Fatalf("owners were told %q", kind)
	}
	bye := svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	var b cloudwire.Bye
	if cloudwire.Unmarshal(bye.B, &b) != nil || !b.Purge || b.Why != cloudwire.ByeRevoked {
		t.Fatalf("bye %+v", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.link.Status().State != "off" {
		if time.Now().After(deadline) {
			t.Fatalf("still %+v after the receipt", h.link.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("cloud.key survived a confirmed purge")
	}
	if st := h.link.Status(); st.PurgedAtMs != 1_791_821_000_000 {
		t.Fatalf("receipt time %d", st.PurgedAtMs)
	}
	// After the bye, nothing but the receipt's ack: no readings.
	got := svc.received()
	for i, env := range got {
		if env.T == cloudwire.MsgBye && i != len(got)-1 {
			t.Fatalf("the box sent %s after bye", got[i+1].T)
		}
	}
}

func TestRevokeWithoutAReceiptKeepsTheKey(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	svc.waitFor(cloudwire.MsgBye, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(h.keyPath); err != nil {
		t.Fatal("cloud.key went before the service confirmed the purge")
	}
	if st := h.link.Status(); st.State != "revoking" {
		t.Fatalf("state %s", st.State)
	}
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); !errors.Is(err, ErrPurgePending) {
		t.Fatalf("re-grant during a pending purge: %v", err)
	}
}

func TestPastTheBudgetOnlyRosterAndByeGo(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.link.mu.Lock()
	h.link.budget.add(h.link.uptimeMs(), budgetBytes)
	h.link.mu.Unlock()
	h.grant(t)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	time.Sleep(2500 * time.Millisecond)
	for _, env := range svc.received() {
		if env.T != cloudwire.MsgRoster {
			t.Fatalf("%s went past the byte budget", env.T)
		}
	}
	if st := h.link.Status(); !st.BudgetReached {
		t.Fatalf("status %+v", st)
	}
}

func TestAGrantWithoutItsKeyReadsAsOff(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	reopen := func(keyPath string) Status {
		link, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: keyPath,
			ServiceKeys: h.link.opts.ServiceKeys})
		if err != nil {
			t.Fatal(err)
		}
		return link.Status()
	}

	// A restore that brought the grant back without its key.
	if st := reopen(filepath.Join(t.TempDir(), "cloud.key")); st.State != "off" || st.Notice != "key_missing" {
		t.Fatalf("missing key: %+v", st)
	}
	// Or with a key that is not the one the grant was made with.
	other := filepath.Join(t.TempDir(), "cloud.key")
	if _, err := createKey(other); err != nil {
		t.Fatal(err)
	}
	if st := reopen(other); st.State != "off" || st.Notice != "key_mismatch" {
		t.Fatalf("other key: %+v", st)
	}
	if st := reopen(h.keyPath); st.State != "on" {
		t.Fatalf("its own key: %+v", st)
	}
}

func TestGrantRefusesWhatFTWCloudMayNotHold(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	for _, scopes := range [][]string{nil, {protocol.ScopeDispatchWrite}, {protocol.ScopeAssetsRead},
		{protocol.ScopeLiveRead, "ftw.everything"}} {
		if err := h.link.Grant(GrantRequest{Scopes: scopes, Policy: "p"}); !errors.Is(err, ErrGrantRequest) {
			t.Errorf("scopes %v: %v", scopes, err)
		}
	}
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}}); !errors.Is(err, ErrGrantRequest) {
		t.Errorf("no policy: %v", err)
	}
	if _, err := os.Stat(h.keyPath); !os.IsNotExist(err) {
		t.Fatal("a refused grant made a key")
	}
}

func TestABuildWithoutAServiceRefusesAGrant(t *testing.T) {
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: "p"}); !errors.Is(err, ErrNoService) {
		t.Fatalf("grant: %v", err)
	}
	if link.Status().Available {
		t.Fatal("status says FTW Cloud is available")
	}
}

func TestBudgetRollsOver24HoursOfUptime(t *testing.T) {
	var b budget
	b.add(0, 100)
	b.add(60_000*60, 50)
	if got := b.total(60_000 * 60); got != 150 {
		t.Fatalf("total %d", got)
	}
	if got := b.total(60_000 * (budgetMinutes + 1)); got != 50 {
		t.Fatalf("after a day: %d", got)
	}
}

func TestChangeTestsIgnoreAgesAndNoise(t *testing.T) {
	age := int64(5)
	a := cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{{ID: "x", SocAgeS: &age, Manual: &cloudwire.Manual{SinceAgeMs: 1}}},
		Feedback: []cloudwire.Feedback{{Kind: "ev", AgeMs: &age}}}
	older := int64(500)
	b := cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{{ID: "x", SocAgeS: &older, Manual: &cloudwire.Manual{SinceAgeMs: 900}}},
		Feedback: []cloudwire.Feedback{{Kind: "ev", AgeMs: &older}}}
	if lpChanged(a, b) {
		t.Fatal("lp differs only in ages and still counts as changed")
	}
	b.Loadpoints[0].PowerW = 90
	if lpChanged(a, b) {
		t.Fatal("90 W of noise counts as a change")
	}
	b.Loadpoints[0].PowerW = 101
	if !lpChanged(a, b) {
		t.Fatal("a power change past the band does not count")
	}
	b.Loadpoints[0].PowerW = 0
	b.Loadpoints[0].Plugged = true
	if !lpChanged(a, b) {
		t.Fatal("a plug-in does not count")
	}
	s1 := cloudwire.Live{Sources: map[string]cloudwire.Source{"a": {State: "live", LastOkMs: 1}}}
	s2 := cloudwire.Live{Sources: map[string]cloudwire.Source{"a": {State: "live", LastOkMs: 9}}}
	if tableKey(s1) != tableKey(s2) {
		t.Fatal("lastOkMs moves the table")
	}
}

// Composing reads the box. The link looks at the source table once a second
// and at the loadpoints once a period, however fast it ticks.
func TestTheLinkReadsTheBoxNoFasterThanItCanSend(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc) // ticks every 20 ms
	h.grant(t)
	svc.waitFor(cloudwire.MsgLP, 5*time.Second)
	h.site.mu.Lock()
	h.site.liveCalls, h.site.lpCalls = 0, 0
	h.site.mu.Unlock()
	time.Sleep(3 * time.Second)
	h.site.mu.Lock()
	live, lp := h.site.liveCalls, h.site.lpCalls
	h.site.mu.Unlock()
	if live > 5 || lp > 1 {
		t.Fatalf("in 3 s the link read live %d times and loadpoints %d times", live, lp)
	}
}

// --------------------------------------------------------------------------
// Session information for phones
// --------------------------------------------------------------------------

// A phone learns the box's key, home id, boot and names over Noise. With them
// it verifies what the service forwards, anchors its times and names its
// sources.
func TestSessionInfoLetsAPhoneCheckAndNameWhatTheServiceForwards(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if s := h.link.SessionInfo(); s != nil {
		t.Fatalf("session information with FTW Cloud off: %+v", s)
	}
	h.grant(t)
	env := svc.waitFor(cloudwire.MsgLive, 5*time.Second)

	s := h.link.SessionInfo()
	if s == nil {
		t.Fatal("no session information with FTW Cloud on")
	}
	if len(s.PK) != cloudwire.PublicKeyBytes || s.Home != cloudwire.HomeID(s.PK) || s.Home != svc.home {
		t.Fatalf("key %x and home %q; the service saw home %q", s.PK, s.Home, svc.home)
	}
	pub, err := cloudwire.ParsePublicKey(s.PK)
	if err != nil || cloudwire.Verify(pub, s.Home, env) != nil {
		t.Fatalf("a forwarded live does not verify with the session key: %v", err)
	}
	var live cloudwire.Live
	if err := cloudwire.Unmarshal(env.B, &live); err != nil {
		t.Fatal(err)
	}
	if len(s.Boot) != cloudwire.BootBytes || !bytes.Equal(s.Boot, live.Boot) {
		t.Fatalf("session boot %x, live boot %x", s.Boot, live.Boot)
	}

	gk := h.link.current().g.GrantKey
	wantSources := map[string]string{
		cloudwire.SourceID(gk, meterName):   meterName,
		cloudwire.SourceID(gk, chargerName): chargerName,
	}
	if !maps.Equal(s.Sources, wantSources) {
		t.Fatalf("sources %v, want %v", s.Sources, wantSources)
	}
	for id := range live.Sources {
		if s.Sources[id] == "" {
			t.Fatalf("live names source %q, which the session cannot name", id)
		}
	}
	if want := map[string]string{cloudwire.LoadpointID(gk, lpName): lpName}; !maps.Equal(s.Loadpoints, want) {
		t.Fatalf("loadpoints %v, want %v", s.Loadpoints, want)
	}
}

// Turning FTW Cloud off withdraws the session information at once, before the
// service confirms the deletion.
func TestSessionInfoGoesWithTheGrant(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	if h.link.SessionInfo() == nil {
		t.Fatal("no session information with FTW Cloud on")
	}
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	if s := h.link.SessionInfo(); s != nil || h.link.Status().State != "revoking" {
		t.Fatalf("while revoking: %+v", s)
	}
}

func TestAvailableFollowsTheServiceKeys(t *testing.T) {
	svc := newFakeService(t)
	if !newHarness(t, svc).link.Available() {
		t.Fatal("a build with a service key says FTW Cloud is unavailable")
	}
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if link.Available() {
		t.Fatal("a build without a service key says FTW Cloud is available")
	}
}

// --------------------------------------------------------------------------
// Statistics
// --------------------------------------------------------------------------

func (h *harness) record(t *testing.T) grantRecord {
	t.Helper()
	g, err := loadJSON[grantRecord](h.kv, grantKey)
	if err != nil || g == nil {
		t.Fatalf("grant record: %v", err)
	}
	return *g
}

func (h *harness) nextRoster(t *testing.T) cloudwire.Roster {
	t.Helper()
	var r cloudwire.Roster
	if err := cloudwire.Unmarshal(h.svc.waitFor(cloudwire.MsgRoster, 3*time.Second).B, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Statistics are a second consent under a grant already on. The box draws a
// statsId once, keeps it while statistics stay on, tells the service through
// the roster and sends no statistics yet.
func TestStatisticsAreASecondConsentUnderTheGrant(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Stats(StatsRequest{Policy: "stats-2026-10"}); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("statistics with FTW Cloud off: %v", err)
	}
	h.grant(t)
	first := h.nextRoster(t)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	if err := h.link.Stats(StatsRequest{}); !errors.Is(err, ErrGrantRequest) {
		t.Fatalf("statistics without a policy: %v", err)
	}

	if err := h.link.Stats(StatsRequest{Policy: "stats-2026-10", By: "app:aaaa1111"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if strings.Join(r.Grant.Purposes, ",") != "serve,stats" || r.RosterSeq <= first.RosterSeq {
		t.Fatalf("roster after statistics on: purposes %v, seq %d after %d", r.Grant.Purposes, r.RosterSeq, first.RosterSeq)
	}
	if st := h.link.Status(); strings.Join(st.Purposes, ",") != "serve,stats" {
		t.Fatalf("status purposes %v", st.Purposes)
	}
	g := h.record(t)
	if len(g.StatsID) != statsIDBytes || g.StatsPolicy != "stats-2026-10" || g.Policy != "2026-10" {
		t.Fatalf("grant record %+v", g)
	}

	// Asked again, the same id.
	if err := h.link.Stats(StatsRequest{Policy: "stats-2026-11"}); err != nil {
		t.Fatal(err)
	}
	if again := h.record(t); !bytes.Equal(again.StatsID, g.StatsID) || again.StatsPolicy != "stats-2026-11" {
		t.Fatalf("statsId %x became %x", g.StatsID, again.StatsID)
	}

	// A restart reads it back.
	reopened, err := New(Options{Site: h.site, Roster: fakeRoster{}, KV: h.kv, KeyPath: h.keyPath,
		ServiceKeys: h.link.opts.ServiceKeys})
	if err != nil {
		t.Fatal(err)
	}
	if st := reopened.Status(); strings.Join(st.Purposes, ",") != "serve,stats" {
		t.Fatalf("after a restart: %+v", st)
	}

	for _, env := range svc.received() {
		if env.T == "stats" {
			t.Fatal("the box sent statistics")
		}
	}
}

// Turning statistics off forgets the statsId; turning them on again draws a
// new one, because the service refuses an id it has deleted.
func TestStoppingStatisticsForgetsTheID(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.StopStats("lan"); err != nil {
		t.Fatalf("statistics off with FTW Cloud off: %v", err)
	}
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.Stats(StatsRequest{Policy: "p"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	old := h.record(t).StatsID

	if err := h.link.StopStats("lan"); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if strings.Join(r.Grant.Purposes, ",") != "serve" {
		t.Fatalf("roster after statistics off: %v", r.Grant.Purposes)
	}
	if g := h.record(t); len(g.StatsID) != 0 || g.StatsPolicy != "" || strings.Join(g.Purposes, ",") != "serve" {
		t.Fatalf("grant record after statistics off: %+v", g)
	}
	if err := h.link.StopStats("lan"); err != nil {
		t.Fatalf("statistics off twice: %v", err)
	}

	if err := h.link.Stats(StatsRequest{Policy: "p"}); err != nil {
		t.Fatal(err)
	}
	if renewed := h.record(t).StatsID; len(renewed) != statsIDBytes || bytes.Equal(renewed, old) {
		t.Fatalf("statsId %x after %x", renewed, old)
	}
}

func TestNoStatisticsWhileTheCopyIsBeingDeleted(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	if err := h.link.Revoke("test"); err != nil {
		t.Fatal(err)
	}
	if err := h.link.Stats(StatsRequest{Policy: "p"}); !errors.Is(err, ErrPurgePending) {
		t.Fatalf("statistics while revoking: %v", err)
	}
}

func TestABuildWithoutAServiceRefusesStatistics(t *testing.T) {
	link, err := New(Options{Site: &fakeSite{}, Roster: fakeRoster{}, KV: &memKV{},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Stats(StatsRequest{Policy: "p"}); !errors.Is(err, ErrNoService) {
		t.Fatalf("statistics: %v", err)
	}
}
