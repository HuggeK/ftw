package cloudlink

import (
	"crypto/ecdsa"
	"errors"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudTermsIncludeStatisticsAndCannotOptOutSeparately(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if r.Grant.TermsPolicy != cloudwire.CloudTermsPolicy || r.Grant.StatsPolicy != cloudwire.StatsPolicy || len(r.Grant.StatsID) != 16 || r.Grant.StatsSinceMs <= 0 {
		t.Fatal("Cloud did not include the accepted statistics scope")
	}
	if err := h.link.StopStats("owner"); !errors.Is(err, ErrCloudNeedsStatistics) {
		t.Fatal("Cloud could opt out independently", err)
	}
	if !h.link.Status().CloudTermsAccepted || !h.link.Status().StatisticsActive {
		t.Fatal("new terms not active")
	}
}

func TestLocalStatisticsPublishesNoRemoteAccess(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy, By: "local owner"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if len(r.Devices) != 0 || len(r.Pending) != 0 || len(r.Noise) != 0 || len(r.WebPush) != 0 || len(r.Grant.Scopes) != 0 || len(r.Grant.Purposes) != 1 || r.Grant.Purposes[0] != protocol.CloudPurposeStats {
		t.Fatal("statistics-only exposed Cloud access", r)
	}
	if h.link.TakesTun() || h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() {
		t.Fatal("local statistics enabled Cloud functions")
	}
	if err := h.link.SendTun(cloudwire.Tun{}, false); !errors.Is(err, ErrNoUplink) {
		t.Fatal("statistics-only queued a tunnel", err)
	}
	if !h.link.Status().StatisticsOnly || h.link.Status().State != "statistics" {
		t.Fatal("local statistics labelled Cloud")
	}
}

func TestDecliningLocalStatisticsRecordsTheAnswerWithoutCreatingALink(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if !h.link.Status().StatisticsDecisionRequired {
		t.Fatal("an unanswered choice looked accepted")
	}
	if err := h.link.StopStats("local owner"); err != nil {
		t.Fatal(err)
	}
	s := h.link.Status()
	if s.StatisticsDecisionRequired || s.Link != "off" || s.StatisticsActive {
		t.Fatal("decline did not stay local", s)
	}
}

func TestEndingCloudPreventsAutomaticRelinkFromPairedPhones(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.End("owner"); err != nil {
		t.Fatal(err)
	}
	if h.link.TakesTun() || h.link.wantsLink() {
		t.Fatal("disabled Cloud still wanted remote access")
	}
	h.link.RosterChanged()
	disabled, ok := h.link.opts.KV.LoadConfig(disabledKey)
	if !ok || disabled != "true" {
		t.Fatal("off choice was not durable")
	}
}

func TestDailyReportRequiresCurrentAnswerAndHonoursDurableOff(t *testing.T) {
	kv := &memKV{}
	if StatisticsChoice(kv) {
		t.Fatal("unanswered box may send")
	}
	g := grantRecord{StatsPolicy: "ftw-project-stats-2026-10-v1", StatsID: make([]byte, 16)}
	if err := saveJSON(kv, grantKey, g); err != nil {
		t.Fatal(err)
	}
	if StatisticsChoice(kv) {
		t.Fatal("old choice may send new report")
	}
	g.StatsPolicy = cloudwire.StatsPolicy
	if err := saveJSON(kv, grantKey, g); err != nil {
		t.Fatal(err)
	}
	if !StatisticsChoice(kv) {
		t.Fatal("current answer may not send")
	}
	// This flag is saved first, even if writing a later purge record fails.
	if err := kv.SaveConfig(disabledKey, "true"); err != nil {
		t.Fatal(err)
	}
	if StatisticsChoice(kv) {
		t.Fatal("durable Cloud off did not stop daily report")
	}
}

func TestStatisticsClockStartsAfterNTPWithoutBackfill(t *testing.T) {
	now := time.Date(2026, 10, 17, 13, 45, 0, 0, time.UTC)
	synced := false
	kv := &memKV{}
	g := grantRecord{StatsPolicy: cloudwire.StatsPolicy, StatsID: make([]byte, 16)}
	l := &Link{log: slog.Default(), opts: Options{KV: kv, Now: func() time.Time { return now }, ClockSynced: func() bool { return synced }}, grant: &g}
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != 0 {
		t.Fatal("an unknown clock started collection")
	}
	synced = true
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != now.UnixMilli() {
		t.Fatal("clock backfilled the earlier choice")
	}
	now = now.Add(time.Hour)
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != now.Add(-time.Hour).UnixMilli() {
		t.Fatal("clock moved on reconnect")
	}
}

type failingChoiceKV struct {
	KV
	armed *atomic.Bool
	key   string
}

func (f failingChoiceKV) SaveConfig(key, value string) error {
	if f.armed.Load() && key == f.key {
		return errors.New("choice storage unavailable")
	}
	return f.KV.SaveConfig(key, value)
}

func TestWholeCloudOffStopsImmediatelyWhenLaterStorageFails(t *testing.T) {
	for _, failKey := range []string{statsDecisionKey, linkKey, grantKey} {
		t.Run(failKey, func(t *testing.T) {
			var armed atomic.Bool
			svc := newFakeService(t)
			h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.KV = failingChoiceKV{KV: o.KV, armed: &armed, key: failKey} })
			if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
				t.Fatal(err)
			}
			h.nextRoster(t)
			armed.Store(true)
			if err := h.link.End("owner"); err == nil {
				t.Fatal("storage failure was not returned")
			}
			if h.link.TakesTun() || h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() || StatisticsChoice(h.kv) {
				t.Fatal("a failed follow-up write left Cloud sharing active")
			}
			if disabled, _ := h.kv.LoadConfig(disabledKey); disabled != "true" {
				t.Fatal("off choice was not saved")
			}
		})
	}
}
