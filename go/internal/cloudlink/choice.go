package cloudlink

import (
	"errors"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

var ErrCloudNeedsStatistics = errors.New("cloudlink: statistics are part of FTW Cloud; turn FTW Cloud off to stop contributing")

// includeStatistics records the exact scope the owner read. Service approval
// still gates every report. The caller holds mu.
func (l *Link) includeStatistics(g *grantRecord, fresh bool) error {
	if fresh || g.StatsPolicy != cloudwire.StatsPolicy || len(g.StatsID) != 16 {
		id, err := randomBytes(statsIDBytes)
		if err != nil {
			return err
		}
		g.StatsID, g.StatsAtMs = id, 0
		if l.clockSynced() {
			g.StatsAtMs = l.opts.Now().UnixMilli()
		}
	}
	g.StatsPolicy = cloudwire.StatsPolicy
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, true)
	return nil
}

func (l *Link) enableLinkChoice() error {
	if err := l.opts.KV.SaveConfig(disabledKey, "false"); err != nil {
		return err
	}
	l.disabled = false
	return nil
}

// startStatisticsOnly reuses the signed uplink, without publishing any phone,
// pairing code, Noise key, live reading or grant for remote access.
func (l *Link) startStatisticsOnly(req StatsRequest) error {
	if err := l.linkLocked(); err != nil {
		return err
	}
	if l.link.Ending {
		return ErrLinkEnding
	}
	id, err := randomBytes(cloudwire.GrantIDBytes)
	if err != nil {
		return err
	}
	key, err := randomBytes(cloudwire.GrantKeyBytes)
	if err != nil {
		return err
	}
	g := grantRecord{ID: id, GrantKey: key, KeyFp: l.link.KeyFp, Policy: req.Policy, By: req.By, Controller: cloudwire.ControllerSourceful, Scopes: []string{}, Purposes: []string{protocol.CloudPurposeStats}}
	if l.clockSynced() {
		g.AtMs = l.opts.Now().UnixMilli()
	}
	if err := l.includeStatistics(&g, true); err != nil {
		return err
	}
	if err := l.enableLinkChoice(); err != nil {
		return err
	}
	return l.storeLocked(g, "local statistics accepted")
}

// End stops all Cloud functions, purges the home and remembers the choice so
// a paired phone or a restart cannot silently turn the link back on.
func (l *Link) End(by string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.opts.KV.SaveConfig(disabledKey, "true"); err != nil {
		return err
	}
	l.disabled = true
	if err := l.opts.KV.SaveConfig(statsDecisionKey, ""); err != nil {
		return err
	}
	if l.link == nil {
		return nil
	}
	rec := *l.link
	rec.Ending = true
	if err := saveJSON(l.opts.KV, linkKey, rec); err != nil {
		return err
	}
	l.link = &rec
	if l.grant != nil {
		g := *l.grant
		g.Revoked, g.By = true, by
		if err := saveJSON(l.opts.KV, grantKey, g); err != nil {
			return err
		}
		l.grant = &g
	}
	l.rosterDue = true
	l.signal()
	return nil
}

// StatisticsChoice is the saved answer for both project reports. The daily
// report must not turn itself on before the owner answers the new question.
func StatisticsChoice(kv KV) bool {
	if kv == nil {
		return false
	}
	if disabled, _ := kv.LoadConfig(disabledKey); disabled == "true" {
		return false
	}
	g, err := loadJSON[grantRecord](kv, grantKey)
	return err == nil && g != nil && !g.Revoked && g.StatsPolicy == cloudwire.StatsPolicy && len(g.StatsID) == 16
}

// An unset clock cannot place a choice in UTC. Start its upload window only
// once the clock is known, never by filling earlier hours after NTP arrives.
func (l *Link) startStatisticsClock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled || l.grant == nil || l.grant.Revoked || l.grant.StatsPolicy != cloudwire.StatsPolicy || l.grant.StatsAtMs != 0 || !l.clockSynced() {
		return nil
	}
	g := *l.grant
	g.StatsAtMs = l.opts.Now().UnixMilli()
	return l.storeLocked(g, "statistics start clock set")
}
