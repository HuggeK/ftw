package drivers

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// A driver's HTTP and TCP reach a service on loopback, but never the box's
// own API port.
func TestADriverNeverReachesTheBoxsOwnAPI(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer api.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	_, port, _ := net.SplitHostPort(api.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	SetSelfAPIPorts(p)
	defer SetSelfAPIPorts()

	client := &http.Client{Transport: newLuaHTTPTransport(false, nil)}
	if _, err := client.Get(api.URL + "/api/agents"); !errors.Is(err, ErrSelfAPI) {
		t.Errorf("the box's API over HTTP: %v", err)
	}
	if resp, err := client.Get(other.URL); err != nil {
		t.Errorf("another service on loopback: %v", err)
	} else {
		resp.Body.Close()
	}
	n := &netTCP{driverName: "t"}
	if err := n.Open(api.Listener.Addr().String()); !errors.Is(err, ErrSelfAPI) {
		t.Errorf("the box's API over TCP: %v", err)
	}
}
