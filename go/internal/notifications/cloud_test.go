package notifications

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

// FTW Cloud on or off reaches the owners even on a box whose stored rules
// predate the two kinds, and stays silent when an owner turned it off.
func TestCloudGrantNoticesAreOnUnlessTurnedOff(t *testing.T) {
	pub := &fakePub{}
	svc, _ := newSvc(pushCfg(config.NotificationRule{Type: PushChargingSessionComplete, Enabled: true}), pub)
	svc.NotifyCloudGrant(PushCloudGranted)
	settle(svc)
	msgs := pub.Messages()
	if len(msgs) != 1 || msgs[0].Kind != PushCloudGranted || msgs[0].Title != "FTW Cloud is on" {
		t.Fatalf("messages %+v", msgs)
	}

	pub = &fakePub{}
	svc, _ = newSvc(pushCfg(config.NotificationRule{Type: PushCloudRevoked, Enabled: false}), pub)
	svc.NotifyCloudGrant(PushCloudRevoked)
	svc.NotifyCloudGrant("charging.connected")
	settle(svc)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("an owner's off switch was ignored, or another kind went through: %d", n)
	}
}

// A security notice goes whether or not the owner turned notifications on,
// and whatever its rule says.
func TestSecurityNoticesPassTheMasterSwitch(t *testing.T) {
	pub := &fakePub{}
	cfg := pushCfg(config.NotificationRule{Type: PushAgentAdded, Enabled: false})
	cfg.Enabled = false
	svc, _ := newSvc(cfg, pub)
	svc.NotifyAgents(PushAgentAdded)
	svc.NotifyAgents(PushOwnerAdded)
	svc.NotifyAgents(PushChargingConnected)
	settle(svc)
	msgs := pub.Messages()
	if len(msgs) != 2 || msgs[0].Kind != PushAgentAdded || msgs[1].Kind != PushOwnerAdded {
		t.Fatalf("messages %+v", msgs)
	}
}
