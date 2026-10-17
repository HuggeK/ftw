package cloudlink

import (
	"crypto/ecdsa"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
	"slices"
	"testing"
)

type browserRows []cloudwire.WebPushRow

func (r browserRows) WebPushRows() ([]cloudwire.WebPushRow, error) { return r, nil }

func TestBrowserNoticesUseTheSignedRosterWhileTheReadableCopyIsOff(t *testing.T) {
	svc := newFakeService(t)
	rows := browserRows{{ID: "sub", Device: "phone", Epoch: 1, Endpoint: "https://push.example/x", CT: []byte("ciphertext"), Auth: "box signature"}}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.WebPush = rows })
	h.setRoster(func(r *fakeRosterState) { r.tun = true })
	r := h.nextRoster(t)
	if len(r.Grant.ID) != 0 || len(r.WebPush) != 1 || !slices.Equal(r.WebPush[0].CT, rows[0].CT) {
		t.Fatal("notice missing or copy grant added")
	}
}
