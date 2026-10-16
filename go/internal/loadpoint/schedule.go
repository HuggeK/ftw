package loadpoint

import (
	"encoding/json"
	"time"

	"github.com/srcfl/ftw/go/internal/units"
)

// Schedule is one loadpoint's saved charge intent. Empty is not stored.
// Recurring rolls the deadline forward; a one-shot does not.
// SurplusUnlockBatSoC arms surplus charging from the home battery's SoC
// even when SurplusOnly is off. Release is threshold minus BatSoCUnlockHyst.
type Schedule struct {
	// FinishAtVehicleLimit keeps the goal pending until the car ends charging.
	// SoC remains the explicit percentage goal when this option is false.
	FinishAtVehicleLimit bool `json:"finish_at_vehicle_limit,omitempty"`
	// Core assigns these when the owner saves a vehicle-limit goal. They
	// keep a completed one-shot goal distinct from a later explicit request.
	IntentID        string  `json:"intent_id,omitempty"`
	FirstDeadlineMS int64   `json:"first_deadline_ms,omitempty"`
	SoC             float64 `json:"soc"`
	// TimeOfDayMinUTC is the ready time as older clients send and read
	// it: the UTC minute of day (0..1439) of that time on today's date.
	// Core answers with today's UTC minute. A request that carries
	// time_of_day_min ignores it.
	TimeOfDayMinUTC int  `json:"time_of_day_min_utc"`
	Recurring       bool `json:"recurring"`
	// Days is an ISO weekday mask, Monday in bit 0. Zero is every day,
	// which is what rows written before the field decode as. The bit
	// is read in the box's zone, not UTC.
	Days                uint8   `json:"days,omitempty"`
	SurplusUnlockBatSoC float64 `json:"surplus_unlock_bat_soc,omitempty"`

	// clock is the ready time in minutes after midnight on the household
	// clock, the box's zone that Days is read in, so a DST change does not
	// move it. JSON carries it as time_of_day_min, in state.db and on the
	// API. clockSet is false for a request or row without it, until Core
	// reads TimeOfDayMinUTC into it.
	clock    int
	clockSet bool
}

// NewSchedule is a goal on the household clock: soc is a fraction, readyBy
// minutes after midnight in the box's zone, days an ISO weekday mask with 0
// for every day of a recurring goal.
func NewSchedule(soc float64, readyByMin int, recurring bool, days uint8, finishAtLimit bool) Schedule {
	s := Schedule{SoC: units.ClampFraction(soc), Recurring: recurring, Days: days & 0x7F,
		FinishAtVehicleLimit: finishAtLimit, clock: readyByMin, clockSet: true}
	return s
}

// BatSoCUnlockHyst is the fraction gap between arm and release for the
// bat-SoC surplus unlock. Armed at threshold, released at threshold −
// 0.05. Tuned to swallow normal Kalman noise on bat SoC (~0.005–0.01)
// without ever flapping the contactor.
const BatSoCUnlockHyst = 0.05

// Normalize folds a persisted or inbound schedule onto 0–1 fractions.
// Old rows stored soc_pct as 0–100; new rows store soc as 0–1.
func (s *Schedule) Normalize() {
	if s == nil {
		return
	}
	s.SoC = units.ClampFraction(units.FractionFromLegacyPercent(s.SoC))
	s.SurplusUnlockBatSoC = units.ClampFraction(units.FractionFromLegacyPercent(s.SurplusUnlockBatSoC))
}

func (s *Schedule) UnmarshalJSON(b []byte) error {
	type alias Schedule
	aux := struct {
		alias
		Clock                  *int     `json:"time_of_day_min"`
		SoCPct                 *float64 `json:"soc_pct"`
		SurplusUnlockBatSoCPct *float64 `json:"surplus_unlock_bat_soc_pct"`
	}{}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*s = Schedule(aux.alias)
	if aux.Clock != nil {
		s.clock, s.clockSet = *aux.Clock, true
	}
	if s.SoC == 0 && aux.SoCPct != nil {
		s.SoC = *aux.SoCPct
	}
	if s.SurplusUnlockBatSoC == 0 && aux.SurplusUnlockBatSoCPct != nil {
		s.SurplusUnlockBatSoC = *aux.SurplusUnlockBatSoCPct
	}
	s.Normalize()
	return nil
}

// MarshalJSON adds the household clock as time_of_day_min once Core has
// one. time_of_day_min_utc stays for older clients, and so that a
// rollback to a Core without the clock still finds a ready time.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type fields Schedule
	row := struct {
		fields
		Clock *int `json:"time_of_day_min,omitempty"`
	}{fields: fields(s)}
	if s.clockSet {
		row.Clock = &s.clock
	}
	return json.Marshal(row)
}

// TimeOfDayMin is the ready time on the household clock, in minutes
// after midnight, and whether the schedule carries one. A decoded
// request may carry a value out of range; the API rejects it.
func (s Schedule) TimeOfDayMin() (int, bool) { return s.clock, s.clockSet }

// HasTarget is true when automatic dispatch must treat the plan as a
// floor. Surplus may add to it and must not throttle it.
func (s Schedule) HasTarget() bool { return s.SoC > 0 || s.FinishAtVehicleLimit }

// Empty reports whether the schedule carries no operator intent. The
// persistence layer writes nothing on Empty so a stale-loadpoint
// schedule on disk is naturally GC'd when the operator clears it via
// the API.
func (s Schedule) Empty() bool {
	return !s.FinishAtVehicleLimit && s.SoC == 0 && s.TimeOfDayMinUTC == 0 && s.clock == 0 && !s.Recurring && s.SurplusUnlockBatSoC == 0
}

// readClock fixes the household clock from TimeOfDayMinUTC on now's
// date, once, so that later DST changes leave the ready time alone. A
// clock out of range is read again. It reports whether it read one.
func (s *Schedule) readClock(now time.Time, loc *time.Location) bool {
	if s.clockSet && s.clock >= 0 && s.clock < 1440 {
		return false
	}
	s.clock, s.clockSet = clockFromUTC(now, s.TimeOfDayMinUTC, zone(loc)), true
	return true
}

// clockAt is the household clock of the ready time. A schedule Core has
// not read yet takes TimeOfDayMinUTC on now's date.
func (s Schedule) clockAt(now time.Time, loc *time.Location) int {
	s.readClock(now, loc)
	return s.clock
}

// withoutClock is s without the household clock, for comparing a goal
// with a copy saved before Core kept the clock, and for a cleared goal.
func (s Schedule) withoutClock() Schedule {
	s.clock, s.clockSet = 0, false
	return s
}

// NextDeadlineUTC is the next time the household clock shows the ready
// time, on a day Days allows, strictly after now. The clock and the
// weekday are read in loc; nil is time.Local. A zero mask is every day.
func (s Schedule) NextDeadlineUTC(now time.Time, loc *time.Location) time.Time {
	loc = zone(loc)
	clock := s.clockAt(now, loc)
	days := s.Days & 0x7F
	y, mo, d := now.In(loc).Date()
	// A non-zero mask allows one of any seven dates, so this ends.
	for i := 0; ; i++ {
		// time.Weekday counts Sunday=0; the mask counts Monday=0 (ISO).
		iso := (int(time.Date(y, mo, d+i, 12, 0, 0, 0, time.UTC).Weekday()) + 6) % 7
		if days != 0 && days&(1<<iso) == 0 {
			continue
		}
		if t := wallClock(y, mo, d+i, clock, loc); t.After(now) {
			return t.UTC()
		}
	}
}

// wallClock is the first instant on the date y-mo-d at which the clock
// in loc shows clock minutes or later. A time a DST change skips gives
// the end of the skipped hour; a time it repeats gives the first one.
// Either way the car is ready no later than the owner asked.
func wallClock(y int, mo time.Month, d, clock int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, clock/60, clock%60, 0, 0, loc)
	start, end := t.ZoneBounds()
	if got := t.Hour()*60 + t.Minute(); got != clock {
		if got > clock {
			return start
		}
		return end
	}
	_, offset := t.Zone()
	_, before := start.Add(-time.Second).Zone()
	if earlier := t.Add(-time.Duration(before-offset) * time.Second); before > offset && earlier.Before(start) {
		return earlier
	}
	return t
}

// utcMinuteToday is the household clock as clients read it: the UTC
// minute of day of that time on now's date in loc.
func utcMinuteToday(now time.Time, clock int, loc *time.Location) int {
	loc = zone(loc)
	y, mo, d := now.In(loc).Date()
	u := wallClock(y, mo, d, clock, loc).UTC()
	return u.Hour()*60 + u.Minute()
}

// clockFromUTC reads a UTC minute of day the way clients make it: as the
// time on now's date in loc that has that UTC minute. On the day a
// repeated hour gives two such times, the first wins.
func clockFromUTC(now time.Time, minUTC int, loc *time.Location) int {
	minUTC = ((minUTC % 1440) + 1440) % 1440
	y, mo, d := now.In(loc).Date()
	start := wallClock(y, mo, d, 0, loc)
	su := start.UTC()
	t := time.Date(su.Year(), su.Month(), su.Day(), minUTC/60, minUTC%60, 0, 0, time.UTC)
	if t.Before(start) {
		t = t.Add(24 * time.Hour)
	}
	l := t.In(loc)
	return l.Hour()*60 + l.Minute()
}

func zone(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}
