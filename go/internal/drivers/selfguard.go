package drivers

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"
)

// No driver reaches the box's own HTTP API. A request from a driver comes from
// inside the box, so the API would take it for the box itself: an owner with
// no sign-in (docs/agents.md, Approval on the box page). Drivers may still
// reach other services on the box or on loopback that their configuration
// names; only the API's ports are closed to them. A proxy that serves the API
// on another port is not covered here: the house password guards the routes
// that give access away, from any address.

// ErrSelfAPI is a driver's connection to the box's own API, refused.
var ErrSelfAPI = errors.New("drivers: a driver may not reach the box's own API")

var selfAPI struct {
	mu    sync.Mutex
	ports []int
	addrs []net.IP
	at    time.Time
}

// SetSelfAPIPorts names the ports the box's HTTP API listens on.
func SetSelfAPIPorts(ports ...int) {
	selfAPI.mu.Lock()
	defer selfAPI.mu.Unlock()
	selfAPI.ports = slices.Clone(ports)
}

// selfAddrsLocked is the box's own addresses, read again each minute. The
// caller holds the mutex.
func selfAddrsLocked() []net.IP {
	if time.Since(selfAPI.at) < time.Minute && selfAPI.addrs != nil {
		return selfAPI.addrs
	}
	selfAPI.addrs = []net.IP{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				selfAPI.addrs = append(selfAPI.addrs, n.IP)
			}
		}
	}
	selfAPI.at = time.Now()
	return selfAPI.addrs
}

// isSelfAPI reports whether addr is the box's own API: an API port on a
// loopback, unspecified or own address.
func isSelfAPI(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	selfAPI.mu.Lock()
	defer selfAPI.mu.Unlock()
	if !slices.Contains(selfAPI.ports, tcp.Port) {
		return false
	}
	if tcp.IP.IsLoopback() || tcp.IP.IsUnspecified() {
		return true
	}
	return slices.ContainsFunc(selfAddrsLocked(), func(ip net.IP) bool { return ip.Equal(tcp.IP) })
}

// refuseSelf closes conn and refuses it when it reached the box's own API.
// The check runs on the address actually dialled, after any name lookup, so
// no name can point a driver round it.
func refuseSelf(conn net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	if isSelfAPI(conn.RemoteAddr()) {
		_ = conn.Close()
		return nil, ErrSelfAPI
	}
	return conn, nil
}

// guardDial wraps a dial function with refuseSelf.
func guardDial(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return refuseSelf(dial(ctx, network, addr))
	}
}
