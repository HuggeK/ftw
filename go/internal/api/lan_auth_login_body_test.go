package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stalledLoginBody struct {
	entered chan struct{}
	release chan struct{}
}

func (b *stalledLoginBody) Read(p []byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}
func (b *stalledLoginBody) Close() error { return nil }

func TestLoginBodyDoesNotHoldConfigLock(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	enableStoredLANAuth(t, srv)
	body := &stalledLoginBody{make(chan struct{}), make(chan struct{})}
	req := lanAuthRequest(http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210", "")
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 100
	req.Body = body
	done := make(chan struct{})
	go func() { srv.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	writerDone := make(chan struct{})
	writerStatus := 0
	defer func() {
		close(body.release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("login cleanup timed out")
		}
		select {
		case <-writerDone:
		case <-time.After(2 * time.Second):
			t.Error("writer cleanup timed out")
		}
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("login did not start reading body")
	}
	go func() {
		rr := postLANAuthJSON(srv, http.MethodPost, "http://ftw.local:8080/api/auth/password", "127.0.0.1:43210", `{"enabled":true}`)
		writerStatus = rr.Code
		close(writerDone)
	}()
	select {
	case <-writerDone:
		if writerStatus != http.StatusOK {
			t.Fatalf("owner settings write status=%d", writerStatus)
		}
	case <-time.After(time.Second):
		t.Fatal("unauthenticated login body blocks owner settings write")
	}
}
