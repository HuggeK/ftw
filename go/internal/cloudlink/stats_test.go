package cloudlink

import (
	"context"
	"crypto/ecdsa"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
	"testing"
	"time"
)

type statsSite struct{ *fakeSite }

func (s statsSite) Statistics(_ context.Context, now time.Time) (cloudwire.Stats, bool) {
	return cloudwire.Stats{HourMS: now.UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli(), Metrics: map[string]int64{"pv_generation_wh": 120}}, true
}
func TestStatisticsSendsSignedClosedHourOnlyWithCurrentConsent(t *testing.T) {
	svc := newFakeService(t)
	svc.statsPolicy = cloudwire.StatsPolicy
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) {
		o.Site = statsSite{&fakeSite{}}
		o.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	})
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.Stats(StatsRequest{Policy: "old-policy"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	for _, e := range svc.received() {
		if e.T == cloudwire.MsgStats {
			t.Fatal("old consent sent statistics")
		}
	}
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	e := svc.waitFor(cloudwire.MsgStats, 3*time.Second)
	var report cloudwire.Stats
	if err := cloudwire.Unmarshal(e.B, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Valid(time.Now().UnixMilli()) || len(r.Grant.StatsID) != 16 || r.Grant.StatsPolicy != cloudwire.StatsPolicy {
		t.Fatalf("invalid report or roster %+v %+v", report, r.Grant)
	}
	if !h.link.Status().StatisticsActive {
		t.Fatal("current consent inactive")
	}
	if err := h.link.StopStats("box"); err != nil {
		t.Fatal(err)
	}
	r = h.nextRoster(t)
	if len(r.Grant.StatsID) != 0 || h.link.Status().StatisticsActive {
		t.Fatal("withdrawn consent remained active")
	}
}

func TestStatisticsWaitsForTheServicesApprovedPolicy(t *testing.T) {
	svc := newFakeService(t)
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) {
		o.Site = statsSite{&fakeSite{}}
		o.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	})
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	if h.link.Status().StatisticsReady {
		t.Fatal("unapproved service declared ready")
	}
	for _, e := range svc.received() {
		if e.T == cloudwire.MsgStats {
			t.Fatal("statistics sent without service approval")
		}
	}
}
