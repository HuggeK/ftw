package cloudlink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// closeWith ends a connection as the service does, with a close code and a
// reason. The caller's read loop takes the box's answer.
func closeWith(conn uplinkConn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

// shardCert is a certificate for cloud.test and s2.cloud.test that is its
// own root.
func shardCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cloud.test"},
		DNSNames:  []string{"cloud.test", "s2.cloud.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// shardNet dials each name at the fake service that plays it, whatever the
// port, and counts the dials per name. A name it does not know fails.
type shardNet struct {
	mu    sync.Mutex
	at    map[string]string
	dials map[string]int
	seq   []string
}

// order is every name dialled, in order.
func (n *shardNet) order() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.seq...)
}

func (n *shardNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.dials[host]++
	n.seq = append(n.seq, host)
	to, ok := n.at[host]
	n.mu.Unlock()
	if !ok {
		return nil, errors.New("no such host")
	}
	return (&net.Dialer{}).DialContext(ctx, network, to)
}

func (n *shardNet) count(host string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[host]
}

// twoShards is cloud.test, which does not hold the home and names
// s2.cloud.test, and s2.cloud.test, which does. Both carry the same service
// key, as every instance does.
func twoShards(t *testing.T) (s1, s2 *fakeService, nw *shardNet, pool *x509.CertPool) {
	cert, pool := shardCert(t)
	s1, s2 = newFakeServiceCert(t, &cert), newFakeServiceCert(t, &cert)
	s2.key = s1.key
	s1.wrongShard = "s2.cloud.test"
	nw = &shardNet{at: map[string]string{
		"cloud.test":    s1.srv.Listener.Addr().String(),
		"s2.cloud.test": s2.srv.Listener.Addr().String(),
	}, dials: map[string]int{}}
	return s1, s2, nw, pool
}

// stop ends a harness's link before its test does; the cleanup still finds
// the link's result.
func stop(h *harness) {
	h.cancel()
	err := <-h.done
	h.done <- err
}

func shardBox(t *testing.T, svc *fakeService, nw *shardNet, pool *x509.CertPool, kv *memKV, keyPath string) *harness {
	return newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, kv, func(o *Options) {
		o.Endpoint, o.RootCAs, o.NetDial = "wss://cloud.test:443/v1/box", pool, nw.dial
		if keyPath != "" {
			o.KeyPath = keyPath
		}
		o.Now = fastClock()
	})
}

// fastClock moves 16 s each time it is read, so three failed dials span
// the 30 s window that lets a box forget its kept host, without the test
// waiting for it.
func fastClock() func() time.Time {
	var mu sync.Mutex
	t := time.Now()
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(16 * time.Second)
		return t
	}
}

// An instance that does not hold the home names the one that does. The box
// follows once, keeps the host, and dials it first after a restart.
func TestTheBoxFollowsAWrongShardCloseOnceAndKeepsTheHost(t *testing.T) {
	s1, s2, nw, pool := twoShards(t)
	h := shardBox(t, s1, nw, pool, nil, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	s2.waitFor(cloudwire.MsgRoster, 5*time.Second)
	if n := nw.count("cloud.test"); n != 1 {
		t.Fatalf("the default host was dialled %d times, want 1", n)
	}
	h.link.mu.Lock()
	kept := h.link.link.Host
	h.link.mu.Unlock()
	if kept != "s2.cloud.test" {
		t.Fatalf("kept host %q", kept)
	}
	if rec, _ := loadJSON[linkRecord](h.kv, linkKey); rec == nil || rec.Host != "s2.cloud.test" {
		t.Fatalf("stored link %+v", rec)
	}

	// The same box after a restart goes straight to its shard.
	keyPath := h.link.opts.KeyPath
	stop(h)
	before := nw.count("cloud.test")
	h2 := shardBox(t, s1, nw, pool, h.kv, keyPath)
	h2.setRoster(func(r *fakeRosterState) { r.tun = true })
	s2.waitFor(cloudwire.MsgRoster, 5*time.Second)
	if n := nw.count("cloud.test"); n != before {
		t.Fatalf("a restarted box dialled the default host %d more times", n-before)
	}
}

// A redirect off the service's domain is not followed: the box stays with
// the default host and backs off.
func TestTheBoxRefusesAShardHostOffTheServicesDomain(t *testing.T) {
	s1, _, nw, pool := twoShards(t)
	s1.wrongShard = "evil.example"
	nw.at["evil.example"] = nw.at["s2.cloud.test"]
	h := shardBox(t, s1, nw, pool, nil, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	deadline := time.Now().Add(5 * time.Second)
	for nw.count("cloud.test") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if nw.count("cloud.test") < 2 || nw.count("evil.example") != 0 {
		t.Fatalf("dials %v", nw.dials)
	}
	if rec, _ := loadJSON[linkRecord](h.kv, linkKey); rec == nil || rec.Host != "" {
		t.Fatalf("stored link %+v", rec)
	}
}

// Two instances that each name the other do not make the box loop: the
// second wrong-shard close sends it back to the default host after backoff.
func TestTwoWrongShardClosesInARowGoBackToTheDefaultHost(t *testing.T) {
	s1, s2, nw, pool := twoShards(t)
	s2.wrongShard = "cloud.test"
	h := shardBox(t, s1, nw, pool, nil, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	time.Sleep(1500 * time.Millisecond)
	// Backoff from 0.5 s with full jitter allows a few rounds in 1.5 s,
	// never the hundreds a loop without it would make.
	if n := nw.count("cloud.test") + nw.count("s2.cloud.test"); n < 3 || n > 20 {
		t.Fatalf("%d dials in 1.5 s", n)
	}
	if nw.count("cloud.test") < 2 {
		t.Fatalf("the box never went back to the default host: %v", nw.dials)
	}
}

// A kept host that stops answering is forgotten after
// cloudwire.ShardHostFailures dials, and the default host names the shard
// again.
func TestAKeptHostThatStopsAnsweringIsForgotten(t *testing.T) {
	s1, s2, nw, pool := twoShards(t)
	kv := &memKV{}
	h := shardBox(t, s1, nw, pool, kv, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	s2.waitFor(cloudwire.MsgRoster, 5*time.Second)
	keyPath := h.link.opts.KeyPath
	stop(h)

	// s2 is gone; s1 now holds the home.
	nw.mu.Lock()
	delete(nw.at, "s2.cloud.test")
	nw.mu.Unlock()
	s1.mu.Lock()
	s1.wrongShard = ""
	s1.mu.Unlock()
	h2 := shardBox(t, s1, nw, pool, kv, keyPath)
	h2.setRoster(func(r *fakeRosterState) { r.tun = true })
	s1.waitFor(cloudwire.MsgRoster, 10*time.Second)
	if n := nw.count("s2.cloud.test"); n != 1+cloudwire.ShardHostFailures {
		t.Fatalf("the gone host was dialled %d times, want %d", n, 1+cloudwire.ShardHostFailures)
	}

	if rec, _ := loadJSON[linkRecord](kv, linkKey); rec == nil || rec.Host != "" {
		t.Fatalf("stored link %+v", rec)
	}
}

// A service about to stop closes with CloseRestart and a spread. The box
// redials within the spread and does not count the close as a failure, so
// its backoff does not grow.
func TestARestartCloseRedialsWithinTheSpreadWithoutBackoff(t *testing.T) {
	svc := newFakeService(t)
	var mu sync.Mutex
	restarts := 0
	svc.afterHello = func(conn uplinkConn, _ string) {
		mu.Lock()
		restarts++
		n := restarts
		mu.Unlock()
		if n <= 5 {
			closeWith(conn, cloudwire.CloseRestart, "50")
		}
	}
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	// Each spread is at least cloudwire.RestartSpreadMinMs, so five take
	// up to 5 s; five failures in a row would back off up to 15.5 s.
	deadline := time.Now().Add(7 * time.Second)
	for {
		svc.mu.Lock()
		n := svc.conns
		svc.mu.Unlock()
		if n >= 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections in 7 s after five restarts", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.link.mu.Lock()
	attempt := h.link.attempt
	h.link.mu.Unlock()
	if attempt != 0 {
		t.Fatalf("attempt %d after restarts", attempt)
	}
}

func TestSpreadStaysWithinItsMilliseconds(t *testing.T) {
	for range 200 {
		if d := spread(40); d < 0 || d >= 40*time.Millisecond {
			t.Fatalf("spread(40) = %s", d)
		}
	}
	if cloudwire.RestartSpreadMs("0") < cloudwire.RestartSpreadMinMs {
		t.Fatal("a reason of 0 gives no spread")
	}
	if spread(0) != 0 {
		t.Fatal("spread(0) waits")
	}
	if !strings.HasPrefix(cloudwire.DefaultHost, "cloud.") {
		t.Fatal(cloudwire.DefaultHost)
	}
}

// A host kept under another Endpoint, as after a build moved the service, is
// not dialled.
func TestAKeptHostOffTheEndpointsDomainIsNotDialled(t *testing.T) {
	s1, _, nw, pool := twoShards(t)
	s1.wrongShard = ""
	kv := &memKV{}
	h := shardBox(t, s1, nw, pool, kv, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	s1.waitFor(cloudwire.MsgRoster, 5*time.Second)
	h.link.mu.Lock()
	rec := *h.link.link
	rec.Host = "s2.old-cloud.example"
	h.link.link = &rec
	h.link.mu.Unlock()
	if got := h.link.endpoint(); got != "wss://cloud.test:443/v1/box" {
		t.Fatalf("dials %s", got)
	}
}

// After a host the box followed to is forgotten, the default host's next
// redirect is followed at once, not refused as a second hop.
func TestAfterForgettingAHostTheNextRedirectIsFollowed(t *testing.T) {
	s1, _, nw, pool := twoShards(t)
	// s2 never answers.
	nw.mu.Lock()
	delete(nw.at, "s2.cloud.test")
	nw.mu.Unlock()
	h := shardBox(t, s1, nw, pool, nil, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	// cloud.test, then s2 until it is forgotten, then cloud.test again,
	// and the dial after that.
	want := 1 + cloudwire.ShardHostFailures + 2
	deadline := time.Now().Add(15 * time.Second)
	for len(nw.order()) < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := nw.order()
	if len(got) < want {
		t.Fatalf("dials %v", got)
	}
	if got[want-2] != "cloud.test" || got[want-1] != "s2.cloud.test" {
		t.Fatalf("after forgetting s2 the box dialled %v; want the redirect followed at once", got[:want])
	}
}

// Three failed dials within a few seconds, as during a shard's restart, do
// not send the box back to the default host: the window is 30 s.
func TestAShardRestartDoesNotMakeTheBoxForgetItsHost(t *testing.T) {
	s1, s2, nw, pool := twoShards(t)
	kv := &memKV{}
	h := shardBox(t, s1, nw, pool, kv, "")
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	s2.waitFor(cloudwire.MsgRoster, 5*time.Second)
	keyPath := h.link.opts.KeyPath
	stop(h)
	nw.mu.Lock()
	addr := nw.at["s2.cloud.test"]
	delete(nw.at, "s2.cloud.test") // s2 restarts
	nw.mu.Unlock()
	h2 := newHarnessWith(t, s1, []*ecdsa.PublicKey{&s1.key.PublicKey}, kv, func(o *Options) {
		o.Endpoint, o.RootCAs, o.NetDial, o.KeyPath = "wss://cloud.test:443/v1/box", pool, nw.dial, keyPath
	})
	h2.setRoster(func(r *fakeRosterState) { r.tun = true })
	deadline := time.Now().Add(10 * time.Second)
	for nw.count("s2.cloud.test") < 1+cloudwire.ShardHostFailures+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	before := nw.count("cloud.test")
	nw.mu.Lock()
	nw.at["s2.cloud.test"] = addr // s2 is back
	nw.mu.Unlock()
	s2.waitFor(cloudwire.MsgRoster, 20*time.Second)
	if n := nw.count("cloud.test") - before; n != 0 {
		t.Fatalf("the box went to the default host %d times during its shard's restart", n)
	}
}

// A 4503 before hello_ok, again and again, makes the backoff grow: only a
// box that was connected treats it as a restart.
func TestARestartBeforeHelloOKKeepsTheBackoff(t *testing.T) {
	svc := newFakeService(t)
	svc.restartBeforeOK = true
	h := newHarness(t, svc)
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	deadline := time.Now().Add(8 * time.Second)
	for {
		h.link.mu.Lock()
		attempt := h.link.attempt
		h.link.mu.Unlock()
		if attempt >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt %d after 8 s of closes before hello_ok", attempt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
