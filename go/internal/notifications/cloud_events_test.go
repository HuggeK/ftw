package notifications

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/events"
)

// K5: a catalogue sentence carries its kind and its arguments, so a
// transport whose phones write their own words, as FTW Cloud's do, gets the
// facts and not only the English.
func TestACatalogueMessageCarriesItsKindAndArgs(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(pushCfg(config.NotificationRule{
		Type: PushDriverOffline, Enabled: true, ThresholdS: 600, Priority: 4, CooldownS: 3600,
	}), pub)
	last := clk.now()
	clk.advance(800 * time.Second)
	svc.Observe(healthStale(last))
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	m := msgs[0]
	if m.Kind != PushDriverOffline || m.Args["name"] != "ferroamp" || m.Device != "ferroamp" || m.At.IsZero() {
		t.Fatalf("message %+v", m)
	}
}

// hangingPub never answers until its context ends.
type hangingPub struct{ entered chan struct{} }

func (h hangingPub) Publish(ctx context.Context, _ Message) error {
	close(h.entered)
	<-ctx.Done()
	return ctx.Err()
}

// Each transport has its own time: one that hangs does not hold up another.
func TestAHangingTransportDoesNotDelayAnother(t *testing.T) {
	quick := &fakePub{published: make(chan struct{}, 1)}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingInterrupted, Enabled: true, Priority: 4,
	}), hangingPub{entered: make(chan struct{})})
	svc.AddPublisher(quick)
	bus := events.NewBus()
	svc.Subscribe(bus)

	start := time.Now()
	bus.Publish(events.ChargingInterrupted{LoadpointID: "garage", At: start})
	select {
	case <-quick.published:
	case <-time.After(2 * time.Second):
		t.Fatal("a hanging transport held up the other")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the quick transport waited %s", waited)
	}
}

// The FTW Cloud transport only queues, and only messages with a kind.
func TestCloudPublisherQueuesOnlyCatalogueKinds(t *testing.T) {
	var mu sync.Mutex
	var queued []Message
	on := true
	p := CloudPublisher{Queue: func(m Message) bool {
		mu.Lock()
		defer mu.Unlock()
		queued = append(queued, m)
		return on
	}}
	if err := p.Publish(context.Background(), Message{Title: "FTW: test"}); err != ErrNothingToSend {
		t.Fatalf("a template message went to FTW Cloud: %v", err)
	}
	if err := p.Publish(context.Background(), Message{Kind: PushChargingConnected}); err != nil {
		t.Fatal(err)
	}
	on = false
	if err := p.Publish(context.Background(), Message{Kind: PushChargingConnected}); err != ErrNothingToSend {
		t.Fatalf("FTW Cloud off answered %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("queued %d", len(queued))
	}
	if err := (CloudPublisher{}).Publish(context.Background(), Message{Kind: PushChargingConnected}); err != ErrNothingToSend {
		t.Fatalf("no queue answered %v", err)
	}
}

// The plug-in notification asks only when the box needs the household: the
// level is assumed or no goal is set. It decides after a pause, and says
// nothing about a cable already out.
func TestPlugInAsksOnlyWhenTheBoxNeedsTheHousehold(t *testing.T) {
	cases := []struct {
		name  string
		facts ChargingFacts
		ok    bool
		want  int
	}{
		{"assumed level, goal set", ChargingFacts{Plugged: true, HasGoal: true}, true, 1},
		{"known level, no goal", ChargingFacts{Plugged: true, LevelKnown: true}, true, 1},
		{"known level and goal", ChargingFacts{Plugged: true, LevelKnown: true, HasGoal: true}, true, 0},
		{"unplugged meanwhile", ChargingFacts{}, true, 0},
		{"unknown loadpoint", ChargingFacts{}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &fakePub{published: make(chan struct{}, 1)}
			svc, _ := newSvc(pushCfg(config.NotificationRule{Type: PushChargingConnected, Enabled: true}), pub)
			svc.connectedSettle = 0
			svc.SetChargingFacts(func(id string) (ChargingFacts, bool) {
				if id != "garage" {
					t.Errorf("asked about %q", id)
				}
				return c.facts, c.ok
			})
			bus := events.NewBus()
			svc.Subscribe(bus)
			bus.Publish(events.ChargingConnected{LoadpointID: "garage", At: time.Now()})
			if c.want == 1 {
				waitForMsgs(t, pub, 1)
				return
			}
			settle(svc)
			if n := len(pub.Messages()); n != 0 {
				t.Fatalf("sent %d", n)
			}
		})
	}
}

// Goal at risk says the target and the ready time.
func TestGoalAtRiskSaysTargetAndTime(t *testing.T) {
	pub := &fakePub{published: make(chan struct{}, 1)}
	svc, _ := newSvc(pushCfg(config.NotificationRule{Type: PushChargingGoalAtRisk, Enabled: true, Priority: 4}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)
	bus.Publish(events.ChargingGoalAtRisk{LoadpointID: "garage", TargetSoC: 0.8, ByClock: "07:00", At: time.Now()})
	m := waitForMsgs(t, pub, 1)[0]
	if m.Kind != PushChargingGoalAtRisk || m.LoadpointID != "garage" || m.Args["target"] != "80" || m.Args["by"] != "07:00" {
		t.Fatalf("message %+v", m)
	}
	if m.Title != "The car may not be ready" || m.Body != "It will not reach 80 % by 07:00 at this pace." {
		t.Fatalf("words %q / %q", m.Title, m.Body)
	}
}
