package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
)

func TestPairingCodeHTTPReturnsTheRealBoxsV3Payload(t *testing.T) {
	id, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "box.key"))
	if err != nil {
		t.Fatal(err)
	}
	id.SetHome(func() string { return "abcdefghijklmnopqrstuvwx23" })
	code, _, err := id.MintSpokenCode(apiauth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.New(&api.Deps{AppEnroll: &appLinkAPI{enroll: id}}).Handler())
	defer s.Close()
	claim := func() *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"kind": "claim", "code": code, "role": "owner"})
		response, err := http.Post(s.URL+"/api/app-link/pairing", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := claim()
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("local claim failed or was cacheable")
	}
	var reply struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(reply.URL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(link.Fragment, ".")
	if link.Scheme != "https" || link.Host != "app.ftw.energy" || link.Path != "/p" || len(parts) != 6 || parts[0] != "v3" {
		t.Fatal("claim did not return the unchanged v3 format")
	}
	enc := base64.RawURLEncoding.EncodeToString
	token, _, pending := id.PendingCode()
	if !pending || parts[1] != enc(id.StaticKey().Public()) || parts[2] != enc(token) || parts[4] != enc(id.RendezvousSecret()) || parts[5] != enc([]byte("abcdefghijklmnopqrstuvwx23")) {
		t.Fatal("payload did not name this box and its full enrollment token")
	}
	if id.AuthorisedCount() != 0 {
		t.Fatal("claim bypassed the Noise enrollment")
	}
	again := claim()
	defer again.Body.Close()
	if again.StatusCode != 400 {
		t.Fatal("short code could be claimed twice")
	}
}
