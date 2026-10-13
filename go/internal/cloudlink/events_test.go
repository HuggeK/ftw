package cloudlink

import (
	"bytes"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// An event leaves signed, numbered and timed, with opaque ids for what it
// concerns and no name in it anywhere.
func TestAnEventLeavesSignedWithoutNames(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead, protocol.ScopeEventsRead)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)

	at := time.Now().Add(-3 * time.Second)
	if !h.link.QueueEvent(EventIn{Kind: protocol.PushDriverOffline, Priority: 4,
		Args: map[string]string{"name": chargerName}, Driver: chargerName, At: at}) {
		t.Fatal("a grant with events refused an event")
	}
	if !h.link.QueueEvent(EventIn{Kind: protocol.PushChargingInterrupted, Priority: 4, Loadpoint: lpName}) {
		t.Fatal("a grant with events refused an event")
	}
	first := svc.waitFor(cloudwire.MsgEvent, 5*time.Second)
	second := svc.waitFor(cloudwire.MsgEvent, 5*time.Second)

	pub, err := cloudwire.ParsePublicKey(h.link.current().pk)
	if err != nil {
		t.Fatal(err)
	}
	var a, b cloudwire.Event
	for _, x := range []struct {
		env cloudwire.Envelope
		out *cloudwire.Event
	}{{first, &a}, {second, &b}} {
		if err := cloudwire.Verify(pub, svc.home, x.env); err != nil {
			t.Fatalf("event not signed by the box: %v", err)
		}
		if err := cloudwire.Unmarshal(x.env.B, x.out); err != nil {
			t.Fatal(err)
		}
	}
	gk := h.link.current().g.GrantKey
	if a.Kind != protocol.PushDriverOffline || a.Severity != cloudwire.SeverityWarning ||
		a.Src != cloudwire.SourceID(gk, chargerName) || len(a.Args) != 0 || a.LP != "" {
		t.Fatalf("driver.offline went as %+v", a)
	}
	if a.Ts == nil || *a.Ts != at.UnixMilli() || a.WallMs == nil || len(a.EventID) != 12 {
		t.Fatalf("driver.offline has no time or id: %+v", a)
	}
	if b.Kind != protocol.PushChargingInterrupted || b.LP != cloudwire.LoadpointID(gk, lpName) ||
		b.Seq <= a.Seq || b.EventID == a.EventID {
		t.Fatalf("charging.interrupted went as %+v after %+v", b, a)
	}
	for _, raw := range [][]byte{first.B, second.B} {
		if bytes.Contains(raw, []byte(chargerName)) || bytes.Contains(raw, []byte(lpName)) {
			t.Fatalf("a name left the house in %x", raw)
		}
	}
}

// Events are their own scope: a grant without it sends none.
func TestAGrantWithoutEventsQueuesNothing(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected}) {
		t.Fatal("an event was queued with FTW Cloud off")
	}
	h.grant(t, protocol.ScopeLiveRead)
	if h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected}) {
		t.Fatal("an event was queued under a grant that leaves events out")
	}
}

// An event that waited longer than a phone would act on it is dropped and
// counted, not sent late.
func TestAnEventTooOldForThePhoneIsDropped(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead, protocol.ScopeEventsRead)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected, At: time.Now().Add(-11 * time.Minute)})
	h.link.QueueEvent(EventIn{Kind: protocol.PushChargingInterrupted})
	env := svc.waitFor(cloudwire.MsgEvent, 5*time.Second)
	var e cloudwire.Event
	if err := cloudwire.Unmarshal(env.B, &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != protocol.PushChargingInterrupted {
		t.Fatalf("sent %s, an event eleven minutes old", e.Kind)
	}
	if st := h.link.Status(); st.EventsDropped != 1 {
		t.Fatalf("dropped %d", st.EventsDropped)
	}
}

// The queue holds the newest events and counts what it pushed out. Here the
// service fails the binding, so nothing drains the queue.
func TestTheEventQueueKeepsTheNewest(t *testing.T) {
	svc := newFakeService(t)
	svc.badEKM = true
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead, protocol.ScopeEventsRead)
	for range eventRing + 3 {
		if !h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected}) {
			t.Fatal("refused")
		}
	}
	h.link.mu.Lock()
	n := len(h.link.events)
	h.link.mu.Unlock()
	if st := h.link.Status(); n != eventRing || st.EventsDropped != 3 {
		t.Fatalf("queue %d, dropped %d", n, st.EventsDropped)
	}
}

// A restart the box chose says so, and the link stops dialling.
func TestByeForAnUpdateLeavesAndTheLinkStops(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t, protocol.ScopeLiveRead)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	for !h.link.Status().Connected {
		time.Sleep(10 * time.Millisecond)
	}

	h.link.Bye(cloudwire.ByeUpdate, 2*time.Second)
	env := svc.waitFor(cloudwire.MsgBye, time.Second)
	var b cloudwire.Bye
	if err := cloudwire.Unmarshal(env.B, &b); err != nil {
		t.Fatal(err)
	}
	if b.Why != cloudwire.ByeUpdate || b.Purge {
		t.Fatalf("bye %+v", b)
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run ended with %v", err)
		}
		h.done <- nil // for the cleanup
	case <-time.After(3 * time.Second):
		t.Fatal("the link kept running after its bye")
	}
	if h.link.QueueEvent(EventIn{Kind: protocol.PushChargingConnected}) {
		t.Fatal("an event was queued after the bye")
	}
	svc.mu.Lock()
	conns := svc.conns
	svc.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the box dialled %d times", conns)
	}
}
