package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/nova"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"path/filepath"
	"testing"
)

func TestBrowserFarewellsStayEncryptedAndFollowTheCurrentDeviceEpoch(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(dir, "app.key"))
	if err != nil {
		t.Fatal(err)
	}
	app, _ := appwire.GenerateKeyPair()
	code, _, _ := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	grant, err := enroll.Authorise(app.Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	key, err := nova.LoadOrCreateIdentity(filepath.Join(dir, "webpush.key"))
	if err != nil {
		t.Fatal(err)
	}
	wp, err := notifications.NewWebPush(key, pushSubscriptionSource{st: st})
	if err != nil {
		t.Fatal(err)
	}
	browser, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	_, err = st.SavePushSubscription("https://push.example/opaque", base64.RawURLEncoding.EncodeToString(browser.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(auth), grant.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	source := webPushFromState{st: st, wp: wp, enroll: enroll}
	rows, err := source.WebPushRows()
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if rows[0].Device != grant.DeviceID || rows[0].Epoch != grant.Epoch || bytes.Contains(rows[0].CT, []byte("title")) {
		t.Fatal("grant or encryption")
	}
	// A revoke makes the row disappear even before the subscription sweep.
	if _, err = enroll.Revoke(grant.DeviceID, appenroll.Presence(true)); err != nil {
		t.Fatal(err)
	}
	rows, err = source.WebPushRows()
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
