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
	// when its driver gives them. Nil is unknown.
	Plugged *bool
	AtSite  *bool
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
			ChargingState  string  `json:"charging_state"`
			ChargeLimit    float64 `json:"charge_limit"`
			ChargeLimitPct float64 `json:"charge_limit_pct"`
			Stale          bool    `json:"stale"`
			Plugged        *bool   `json:"plugged"`
			AtSite         *bool   `json:"at_site"`
		}
		if len(vr.Data) > 0 {
			_ = json.Unmarshal(vr.Data, &meta)
		}
		out = append(out, VehicleObservation{
			Driver:        vr.Driver,
			SoC:           units.ClampFraction(*socValue),
			ChargeLimit:   units.ClampFraction(units.DecodeJSONFraction(meta.ChargeLimit, meta.ChargeLimitPct)),
			ChargingState: meta.ChargingState,
			At:            at,
			Stale:         meta.Stale || cached,
			Plugged:       meta.Plugged,
			AtSite:        meta.AtSite,
			ReportedAt:    vr.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Driver < out[j].Driver })
	return out
}
