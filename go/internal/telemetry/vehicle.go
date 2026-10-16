package telemetry

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/units"
)

// VehicleMaxAge is how long a car's charging state counts as live: what
// wakes the car and what tells the box its goal is done. Picked at 5 min so
// a vehicle driver that has lost contact (asleep car, paired-proxy outage,
// cloud-API throttle) cannot keep an old state live. It no longer ages the
// car's level: a level stays the car's until energy flows (docs/cars.md).
const VehicleMaxAge = 5 * time.Minute

// A vehicle driver may give the car's distance from the site instead of
// at_site. Within SiteNearM the car is here; beyond SiteAwayM it is
// elsewhere; between, the box does not know. The site's position is the
// weather pin, set for forecasts with a click on a map shown at zoom 11, so
// it can be off by a kilometre or more. A car that wrongly reads as
// elsewhere turns the only car into a visitor (docs/cars.md), which is worse
// than not knowing.
const (
	SiteNearM = 300.0
	SiteAwayM = 5000.0
)

// atSiteFromDistance turns a distance from the site into the car's at_site.
// Nil is unknown, as is a distance that is not a finite, non-negative number.
func atSiteFromDistance(m *float64) *bool {
	if m == nil || math.IsNaN(*m) || math.IsInf(*m, 0) || *m < 0 {
		return nil
	}
	switch {
	case *m <= SiteNearM:
		v := true
		return &v
	case *m > SiteAwayM:
		v := false
		return &v
	}
	return nil
}

// pluggedSinceFrom reads plugged_since_ms, the unix ms at which the car's
// source saw its cable take the state plugged says. Zero is unknown, as is a
// time before 2020 or more than a day ahead of now.
func pluggedSinceFrom(ms *float64, now time.Time) time.Time {
	if ms == nil || math.IsNaN(*ms) || math.IsInf(*ms, 0) {
		return time.Time{}
	}
	t := time.UnixMilli(int64(*ms))
	if t.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) || t.After(now.Add(24*time.Hour)) {
		return time.Time{}
	}
	return t
}

// VehicleConnectedRank scores how likely a DerVehicle driver is to be
// the one physically plugged into the loadpoint right now, using the
// charging_state vocabulary every vehicle driver normalizes to (the
// strings below are the canonical values; vendor specifics are
// translated inside each Lua driver). Higher rank = more likely
// connected. Negative = explicitly not connected; caller should skip.
//
// The cars runtime reads a rank of 1 or more as the car's word that a cable
// is in, and 3 as the car charging.
func VehicleConnectedRank(chargingState string) int {
	switch chargingState {
	case "Charging", "Starting":
		return 3 // actively pulling power — definitely this car
	case "NoPower":
		return 2 // plugged but wallbox not delivering yet
	case "Stopped", "Complete":
		return 1 // plugged + idle (charge limit reached, paused, etc.)
	case "Disconnected":
		return -1 // explicitly unplugged — never pick this one
	default:
		return 0 // unknown/missing — usable but de-prioritised
	}
}

// Cached car reports can arrive before any fresh report after a restart.
// Store.SoC intentionally retains only control-trusted observations. Read the
// latest raw report with its explicit cache flag and measured SoC age. It may
// be displayed or anchored in a session with known metering history, but must
// never become a live observation or confirm completion. Never synthesize a
// fresh receipt time.
func cachedVehicleObservation(s *Store, vr *DerReading) (float64, time.Time, bool) {
	var report struct {
		SoC      *float64 `json:"soc"`
		SoCFresh *bool    `json:"soc_fresh"`
	}
	if json.Unmarshal(vr.Data, &report) != nil || report.SoC == nil ||
		report.SoCFresh == nil || *report.SoCFresh {
		return 0, time.Time{}, false
	}
	raw := *report.SoC
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw > 100 {
		return 0, time.Time{}, false
	}
	age, receivedAt, ok := s.LatestMetric(vr.Driver, "vehicle_soc_age_s")
	if !ok || receivedAt.IsZero() || math.IsNaN(age) || math.IsInf(age, 0) ||
		age < 0 || age > (100*365*24*time.Hour).Seconds() {
		return 0, time.Time{}, false
	}
	return units.DecodeJSONFraction(0, raw), receivedAt.Add(-time.Duration(age * float64(time.Second))), true
}

// VehicleObservation is one vehicle driver's newest level report. The cars
// runtime binds each driver to a car (docs/cars.md), so nothing here ranks
// one driver against another or ages a reading out: a level with no energy
// delivered since stays the car's level however old it is.
type VehicleObservation struct {
	Driver        string
	SoC           float64   // bounded [0,1]
	ChargeLimit   float64   // bounded [0,1], 0 when unknown
	ChargingState string    // the canonical vocabulary of VehicleConnectedRank
	At            time.Time // when the car measured SoC
	// Stale is the driver's own stale flag, or a cached replay. Such a
	// level may anchor an estimate, but its charging state is not live.
	Stale bool
	// Plugged and AtSite are the car's own word on its cable and place,
	// when its driver gives them. Nil is unknown. AtSite comes from
	// at_site, or else from site_distance_m.
	Plugged *bool
	AtSite  *bool
	// PluggedSince is when the car's source saw its cable take the state
	// Plugged says, by its own clock: Home Assistant's last_changed, or the
	// first reading that showed it. Zero is unknown.
	PluggedSince time.Time
	// ReportedAt is when the driver last reported anything, which can be
	// later than At: a cable changes without a new level.
	ReportedAt time.Time
}

// Live reports whether the charging state is current enough to act on:
// not stale, and measured within VehicleMaxAge.
func (o VehicleObservation) Live(now time.Time) bool {
	age := now.Sub(o.At)
	return !o.Stale && age >= 0 && age <= VehicleMaxAge
}

// VehicleObservations lists the newest level report of every online vehicle
// driver, sorted by driver name. Every vehicle driver pulls from a network
// trust boundary, so the bounds hold here: SoC and charge limit within
// [0,1] (the store refuses an SoC outside it), an offline driver contributes
// nothing, and a carried value keeps the time the car measured it. A cached
// cloud report, with its measured age, stands in when the driver has no
// fresher level; it is marked stale.
func VehicleObservations(s *Store) []VehicleObservation {
	if s == nil {
		return nil
	}
	var out []VehicleObservation
	for _, vr := range s.ReadingsByType(DerVehicle) {
		if h := s.DriverHealth(vr.Driver); h == nil || !h.IsOnline() {
			continue
		}
		socValue, at := vr.SoC, vr.SoCUpdatedAt
		cached := false
		if value, observedAt, ok := cachedVehicleObservation(s, vr); ok &&
			(socValue == nil || at.IsZero() || !observedAt.Before(at)) {
			socValue, at, cached = &value, observedAt, true
		}
		if socValue == nil || at.IsZero() {
			continue
		}
		var meta struct {
			ChargingState  string   `json:"charging_state"`
			ChargeLimit    float64  `json:"charge_limit"`
			ChargeLimitPct float64  `json:"charge_limit_pct"`
			Stale          bool     `json:"stale"`
			Plugged        *bool    `json:"plugged"`
			PluggedSince   *float64 `json:"plugged_since_ms"`
			AtSite         *bool    `json:"at_site"`
			SiteDistanceM  *float64 `json:"site_distance_m"`
		}
		if len(vr.Data) > 0 {
			_ = json.Unmarshal(vr.Data, &meta)
		}
		atSite := meta.AtSite
		if atSite == nil {
			atSite = atSiteFromDistance(meta.SiteDistanceM)
		}
		var since time.Time
		if meta.Plugged != nil {
			since = pluggedSinceFrom(meta.PluggedSince, vr.UpdatedAt)
		}
		out = append(out, VehicleObservation{
			Driver:        vr.Driver,
			SoC:           units.ClampFraction(*socValue),
			ChargeLimit:   units.ClampFraction(units.DecodeJSONFraction(meta.ChargeLimit, meta.ChargeLimitPct)),
			ChargingState: meta.ChargingState,
			At:            at,
			Stale:         meta.Stale || cached,
			Plugged:       meta.Plugged,
			AtSite:        atSite,
			PluggedSince:  since,
			ReportedAt:    vr.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Driver < out[j].Driver })
	return out
}
