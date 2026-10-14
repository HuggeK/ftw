package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// FTW Cloud's uplink, wired to the box.
//
// Everything here reads. cloudlink may import nothing from the box, so these
// adapters answer its questions from the same sources the app session and the
// HTTP API use: the live snapshot, the loadpoint view GET /api/loadpoints
// composes, and the price store. None of them is handed anything that can
// change state.

// cloudSite is the box's state as the uplink wants it.
type cloudSite struct {
	site   *appSite
	api    *lateAPI
	prices *appPrices

	sessMu   sync.Mutex
	sessions map[string]*plugSession
}

// plugSession numbers plug-ins per loadpoint for the life of this process.
// The phone keys what it holds by boot, so a number per boot is enough to
// tell one charging session from the next.
type plugSession struct {
	plugged bool
	n       int
}

func (c *cloudSite) Live() cloudlink.Live {
	snap := c.site.Snapshot()
	uptime := time.Since(c.site.started).Milliseconds()
	fields, sources := appproto.LiveView(snap, uptime)

	out := cloudlink.Live{
		Fields:            fields,
		Sources:           make(map[string]cloudlink.Source, len(sources)),
		FieldSources:      map[string][]string{},
		DispatchBlockedBy: snap.DispatchBlockedBy,
	}
	for name, s := range sources {
		lastOk := s.LastOkMs
		if s.State == appproto.SourceNever {
			lastOk = -1
		}
		out.Sources[name] = cloudlink.Source{Kind: s.Kind, State: s.State, LastOkUptimeMs: lastOk, StaleAfterMs: s.StaleAfterMs}
	}

	// Each field follows the drivers that produce it. Load is what is left
	// of the grid after every other flow, so it follows all of them.
	meter := siteMeterName(c.site.ctrl, c.site.ctrlMu)
	byType := func(t telemetry.DerType) []string {
		var names []string
		for _, r := range c.site.tel.ReadingsByType(t) {
			names = append(names, r.Driver)
		}
		sort.Strings(names)
		return names
	}
	pv, battery, ev := byType(telemetry.DerPV), byType(telemetry.DerBattery), byType(telemetry.DerEV)
	set := func(fid appproto.Fid, names []string) {
		key := strconv.Itoa(int(fid))
		if _, sent := fields[key]; sent && len(names) > 0 {
			out.FieldSources[key] = names
		}
	}
	var meterOnly []string
	if meter != "" {
		meterOnly = []string{meter}
	}
	set(appproto.FidGridW, meterOnly)
	set(appproto.FidPvW, pv)
	set(appproto.FidBatteryW, battery)
	set(appproto.FidBatterySoc, battery)
	set(appproto.FidEvW, ev)
	set(appproto.FidLoadW, uniqueSorted(meterOnly, pv, battery, ev))
	return out
}

func uniqueSorted(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (c *cloudSite) Loadpoints() (cloudwire.LP, bool) {
	srv := c.api.srv.Load()
	if srv == nil {
		return cloudwire.LP{}, false
	}
	now := time.Now()
	states, feedback, ok := srv.CloudLoadpoints(now)
	if !ok || len(states) == 0 {
		return cloudwire.LP{}, false
	}
	c.site.ctrlMu.Lock()
	rev := c.site.revision.Observe(c.site.ctrl)
	c.site.ctrlMu.Unlock()

	out := cloudwire.LP{ControlRev: rev}
	for _, st := range states {
		out.Loadpoints = append(out.Loadpoints, c.loadpoint(st, now))
	}
	for _, f := range feedback {
		out.Feedback = append(out.Feedback, cloudFeedback(f, now))
	}
	return out, true
}

func (c *cloudSite) session(id string, plugged bool) string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	if c.sessions == nil {
		c.sessions = map[string]*plugSession{}
	}
	s := c.sessions[id]
	if s == nil {
		s = &plugSession{}
		c.sessions[id] = s
	}
	if plugged && !s.plugged {
		s.n++
	}
	s.plugged = plugged
	if !plugged {
		return ""
	}
	return strconv.Itoa(s.n)
}

// loadpoint maps the API's view onto the wire. Capabilities (charge limits,
// phases, battery size) and the vehicle's name are not in the shape.
func (c *cloudSite) loadpoint(st loadpoint.State, now time.Time) cloudwire.Loadpoint {
	lp := cloudwire.Loadpoint{
		ID: st.ID, Src: st.DriverName, Plugged: st.PluggedIn,
		SocSource: st.SoCSource, Recurring: !st.Schedule.Empty(),
		PowerW: roundW(st.CurrentPowerW), SessionWh: roundW(st.DeliveredWhSession),
		Session:         c.session(st.ID, st.PluggedIn),
		CommandedReason: st.CommandedReason, SurplusOnly: st.SurplusOnly,
		GridDeferred: st.GridDeferred, PlanPending: st.PlanPending, Boost: st.BatteryBoost.Active,
	}
	if st.PluggedIn {
		lp.SocPermille = ptr(units.PermilleFromFraction(st.CurrentSoC))
		if st.SoCSource == "vehicle" {
			lp.SocAgeS = ptr(st.VehicleSoCAgeS)
		}
	}
	if st.TargetSoC > 0 {
		lp.TargetPermille = ptr(units.PermilleFromFraction(st.TargetSoC))
	}
	if !st.TargetTime.IsZero() {
		lp.ReadyByMs = ptr(st.TargetTime.UnixMilli())
	}
	if st.CommandedKnown {
		lp.CommandedW = ptr(roundW(st.CommandedW))
	}
	if m := st.Manual; m.Active {
		manual := &cloudwire.Manual{State: m.State, Reason: m.LimitReason, RequestedW: ptr(roundW(m.RequestedW))}
		if m.SinceMs > 0 {
			manual.SinceAgeMs = max(0, now.UnixMilli()-m.SinceMs)
		}
		if m.CommandedW > 0 {
			manual.CommandedW = ptr(roundW(m.CommandedW))
		}
		lp.Manual = manual
	}
	if st.PlanNextStartMs > 0 {
		lp.NextStartMs, lp.NextEndMs = ptr(st.PlanNextStartMs), ptr(st.PlanNextEndMs)
		lp.NextWh = ptr(roundW(st.PlanNextWh))
	}
	return lp
}

func cloudFeedback(f api.ControlFeedback, now time.Time) cloudwire.Feedback {
	out := cloudwire.Feedback{
		Kind: f.Kind, Src: f.Driver, Status: f.Status, Reason: f.Reason,
		Severity: f.Severity, Evidence: f.Evidence, Fresh: f.ReadingsFresh,
	}
	if f.ActualW != nil {
		out.ActualW = ptr(roundW(*f.ActualW))
	}
	if f.ObservedAtMs > 0 {
		out.AgeMs = ptr(max(0, now.UnixMilli()-f.ObservedAtMs))
	}
	return out
}

// Prices is the window from the start of today, on the box's own clock, to
// the last stored slot. Starting at midnight rather than now keeps the hash,
// and so the message, still until new prices land.
func (c *cloudSite) Prices() (cloudwire.Prices, bool) {
	if c.prices == nil {
		return cloudwire.Prices{}, false
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	slots, err := c.prices.Slots(context.Background(), midnight.UnixMilli(), midnight.AddDate(0, 0, 2).UnixMilli())
	if err != nil || len(slots) == 0 {
		return cloudwire.Prices{}, false
	}
	return priceColumns(c.prices.Zone(), c.prices.Currency(), slots), true
}

// priceColumns lays slots out at the shortest slot length. A longer slot's
// price repeats across the columns it covers; a gap is null.
func priceColumns(zone, currency string, slots []appproto.PricePoint) cloudwire.Prices {
	sort.Slice(slots, func(a, b int) bool { return slots[a].StartMs < slots[b].StartMs })
	slotMs := int64(math.MaxInt64)
	for _, s := range slots {
		slotMs = min(slotMs, int64(s.LenMin)*60_000)
	}
	p := cloudwire.Prices{Zone: zone, Currency: currency, T0Ms: slots[0].StartMs, SlotMs: slotMs}
	for _, s := range slots {
		for at := s.StartMs; at < s.StartMs+int64(s.LenMin)*60_000; at += slotMs {
			i := int((at - p.T0Ms) / slotMs)
			for len(p.SpotMinor) < i {
				p.SpotMinor, p.TotalMinor = append(p.SpotMinor, nil), append(p.TotalMinor, nil)
			}
			if len(p.SpotMinor) == i {
				p.SpotMinor = append(p.SpotMinor, ptr(roundW(s.SpotMinor)))
				p.TotalMinor = append(p.TotalMinor, ptr(roundW(s.TotalMinor)))
			}
		}
	}
	return p
}

func roundW(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.Round(v))
}

func ptr[T any](v T) *T { return &v }

// cloudRoster lists paired devices with the scopes the box derives for each:
// a phone's role, or for a watch what its role and its phone's role share.
type cloudRoster struct {
	enroll *appenroll.Identity
}

func (r cloudRoster) Devices() []cloudlink.RosterDevice {
	var out []cloudlink.RosterDevice
	for _, e := range r.enroll.Roster() {
		out = append(out, cloudlink.RosterDevice{ID: e.ID, Key: e.Key, Role: e.Role, Epoch: e.Epoch,
			Scopes: appproto.GrantScopes(e.Role, e.ParentRole)})
	}
	return out
}

// cloudNotices reaches the notifications service once it exists; it is built
// after the link starts.
type cloudNotices struct {
	svc atomic.Pointer[notifications.Service]
}

func (n *cloudNotices) notify(kind string) {
	if svc := n.svc.Load(); svc != nil {
		svc.NotifyCloudGrant(kind)
	}
}

// startCloudLink loads the FTW Cloud grant and keeps the uplink up while an
// owner has it on. It needs the state store, which holds the grant, and
// enrolment, which holds the phones the roster lists; without either it
// returns nil and the routes say FTW Cloud is unavailable.
func startCloudLink(
	ctx context.Context, statePath, build string, st *state.Store, enroll *appenroll.Identity,
	tel *telemetry.Store, ctrl *control.State, ctrlMu *sync.Mutex, revision *control.Revision,
	siteMeterStale time.Duration, priceSvc *prices.Service, gateway *lateAPI, notices *cloudNotices,
) *cloudlink.Link {
	if st == nil || enroll == nil {
		return nil
	}
	site := &cloudSite{api: gateway, site: &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: ctrlMu, revision: revision,
		started: processStarted, siteMeterStale: siteMeterStale,
	}}
	if priceSvc != nil && priceSvc.Zone != "" && priceSvc.Store != nil {
		site.prices = &appPrices{svc: priceSvc}
	}
	link, err := cloudlink.New(cloudlink.Options{
		Site:        site,
		Roster:      cloudRoster{enroll: enroll},
		KV:          st,
		KeyPath:     filepath.Join(filepath.Dir(statePath), "cloud.key"),
		Build:       build,
		Started:     processStarted,
		ClockSynced: cloudlink.ClockSynced,
		Notify:      notices.notify,
	})
	if err != nil {
		slog.Warn("FTW Cloud unavailable", "err", err)
		return nil
	}
	enroll.OnChange(link.RosterChanged)
	go func() {
		if err := link.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("FTW Cloud uplink stopped", "err", err)
		}
	}()
	return link
}

// cloudSession is FTW Cloud as an app session sees it. The app link starts
// first, because the cloud link's roster needs its enrolment, so the link is
// bound here once it exists. A session that opens before then hears no
// cloud.link and no session information, which is what a box without FTW
// Cloud says.
type cloudSession struct {
	link atomic.Pointer[cloudlink.Link]
}

func (c *cloudSession) bind(link *cloudlink.Link) { c.link.Store(link) }

func (c *cloudSession) load() *cloudlink.Link {
	if c == nil {
		return nil
	}
	return c.link.Load()
}

// caps adds cloud.link to an app session's capabilities when this build can
// run the uplink. A build that names no service says nothing.
func (c *cloudSession) caps(caps []string) []string {
	if link := c.load(); link != nil && link.Available() {
		return append(slices.Clip(caps), appproto.CapCloudLink)
	}
	return caps
}

// info is hello_ok's FTW Cloud session information, nil while FTW Cloud is
// off.
func (c *cloudSession) info() *appproto.CloudSession {
	if link := c.load(); link != nil {
		return link.SessionInfo()
	}
	return nil
}

// cloudByeWait bounds how long shutdown waits for the bye to leave.
const cloudByeWait = 2 * time.Second

// cloudEventQueue turns a rendered notification into an event for the
// uplink, which cuts the names out before it signs.
func cloudEventQueue(link *cloudlink.Link) func(notifications.Message) bool {
	return func(m notifications.Message) bool {
		return link.QueueEvent(cloudlink.EventIn{
			Kind: m.Kind, Priority: m.Priority, Args: m.Args,
			Driver: m.Device, Loadpoint: m.LoadpointID, At: m.At,
		})
	}
}

// cloudForAPI hands the link to the API, or nothing: a typed nil in an
// interface would pass the routes' nil check and panic on first use.
func cloudForAPI(link *cloudlink.Link) api.CloudGrant {
	if link == nil {
		return nil
	}
	return link
}
