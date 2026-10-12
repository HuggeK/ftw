package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
)

const childWithoutTZ = "FTW_TEST_CHILD_WITHOUT_TZ"

// The app draws plan times in the zone the hello names. Go calls the host zone
// "Local" when TZ is unset, which no client can load, so this test runs the
// hello again in a child process whose environment has no TZ at all.
func TestTheHelloNamesTheBoxZoneWhenTZIsUnset(t *testing.T) {
	if os.Getenv(childWithoutTZ) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTheHelloNamesTheBoxZoneWhenTZIsUnset$", "-test.count=1")
		cmd.Env = []string{childWithoutTZ + "=1"}
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "TZ=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hello without TZ: %v\n%s", err, out)
		}
		return
	}

	if time.Local.String() != "Local" {
		t.Fatalf("time.Local = %q without TZ, want Go's unnamed Local", time.Local)
	}

	tz := helloBoxZone(t, newAppBoxInfo("ftw-0001", "test"))
	if tz == "" || tz == "Local" {
		t.Fatalf("hello tz = %q, want an IANA name or UTC", tz)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		t.Fatalf("hello tz = %q does not load: %v", tz, err)
	}
}

// helloBoxZone sends one hello through a real handler and returns the zone
// the box answered with. The hello reads only the box info, so the other
// ports stay empty.
func helloBoxZone(t *testing.T, info appBoxInfo) string {
	t.Helper()
	sent := &sentFrames{}
	h, err := appproto.New(appproto.Config{
		Clock:  appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:   &appSite{},
		Info:   info,
		Modes:  &appModes{},
		Plans:  &appPlans{},
		Codec:  appuplink.Codec(),
		Sender: sent,
		Caller: apiauth.Caller{
			Kind:   apiauth.KindApp,
			Role:   apiauth.RoleOwner,
			Scopes: appproto.ScopesForRole(apiauth.RoleOwner),
		},
		Grants: ownerGrant{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	hello, err := appproto.Marshal(appproto.Hello{Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax}})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := appuplink.Codec().EncodeFrame(appproto.Frame{
		Lane:     appproto.LaneControl,
		Bucket:   512,
		Envelope: appproto.Envelope{T: appproto.MsgHello, B: hello},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}

	for _, raw := range sent.all() {
		f, err := appuplink.Codec().DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		if f.Envelope.T != appproto.MsgHelloOK {
			continue
		}
		var ok appproto.HelloOK
		if err := appproto.Unmarshal(f.Envelope.B, &ok); err != nil {
			t.Fatal(err)
		}
		return ok.Box.TZ
	}
	t.Fatal("the box did not answer the hello")
	return ""
}

type sentFrames struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *sentFrames) Send(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, append([]byte(nil), raw...))
	return nil
}

func (s *sentFrames) all() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.frames...)
}

type ownerGrant struct{}

func (ownerGrant) Grant() (string, uint64, bool) { return apiauth.RoleOwner, 1, true }
