package loadpoint

import (
	"encoding/json"
	"testing"
	"time"
)

// The ready time is a time on the household's clock. Sweden leaves
// summer time on 2026-10-25 and returns on 2027-03-28; 07:00 must stay
// 07:00 across both, which moves it an hour in UTC.

func dstManager(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	m.SetLocation(stockholm(t))
	m.SetNowFn(func() time.Time { return *now })
	return m
}

func utc(y int, mo time.Month, d, h, min int) time.Time {
	return time.Date(y, mo, d, h, min, 0, 0, time.UTC)
}

func TestReadyTimeKeepsWallClockOverDSTChanges(t *testing.T) {
	loc := stockholm(t)
	for _, c := range []struct {
		name   string
		saved  time.Time
		minUTC int         // what a client sends for 07:00 on the save date
		want   []time.Time // the next three deadlines around the change
	}{
		{"autumn 2026-10-25", utc(2026, 10, 12, 10, 0), 5 * 60,
			[]time.Time{utc(2026, 10, 24, 5, 0), utc(2026, 10, 25, 6, 0), utc(2026, 10, 26, 6, 0)}},
		{"spring 2027-03-28", utc(2027, 2, 1, 10, 0), 6 * 60,
			[]time.Time{utc(2027, 3, 27, 6, 0), utc(2027, 3, 28, 5, 0), utc(2027, 3, 29, 5, 0)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := c.saved
			m := dstManager(t, &now)
			m.SetSchedule("garage", Schedule{SoC: 0.8, TimeOfDayMinUTC: c.minUTC, Recurring: true})
			for _, want := range c.want {
				now = want.Add(-10 * time.Hour) // the evening before
				m.RollSchedules(now)
				st, _ := m.State("garage")
				if !st.TargetTime.Equal(want) {
					t.Fatalf("deadline = %v, want %v", st.TargetTime, want)
				}
				if got := st.TargetTime.In(loc).Format("15:04"); got != "07:00" {
					t.Fatalf("deadline on %s is %s on the household clock, want 07:00", want.Format("2006-01-02"), got)
				}
			}
		})
	}
}

// Clients turn the UTC minute into a local time with today's offset, so
// Core answers with today's UTC minute: 07:00 reads as 05:00 UTC in
// summer and 06:00 UTC in winter.
func TestClientsReadTodaysUTCMinute(t *testing.T) {
	now := utc(2026, 11, 1, 10, 0) // CET
	m := dstManager(t, &now)
	var s Schedule
	if err := json.Unmarshal([]byte(`{"soc":0.8,"time_of_day_min_utc":360,"recurring":true}`), &s); err != nil {
		t.Fatal(err)
	}
	m.SetSchedule("garage", s)
	for _, c := range []struct {
		now  time.Time
		want int
	}{
		{utc(2026, 11, 1, 10, 0), 6 * 60},
		{utc(2027, 3, 27, 10, 0), 6 * 60},
		{utc(2027, 3, 29, 10, 0), 5 * 60},
		{utc(2027, 10, 30, 10, 0), 5 * 60},
		{utc(2027, 11, 1, 10, 0), 6 * 60},
	} {
		now = c.now
		st, _ := m.State("garage")
		if st.Schedule.TimeOfDayMinUTC != c.want {
			t.Errorf("%s: time_of_day_min_utc = %d, want %d", c.now.Format("2006-01-02"), st.Schedule.TimeOfDayMinUTC, c.want)
		}
	}
}

// The weekday is the household's. A Monday-only 00:30 goal falls on
// Sunday in UTC. Kept as a UTC minute it became Sunday 23:30 after the
// autumn change, so the mask skipped it and the car was due on Monday
// at 23:30, almost a day late.
func TestWeekdayMaskAtHalfPastMidnightOverDSTChanges(t *testing.T) {
	loc := stockholm(t)
	const monday = 1 << 0
	for _, c := range []struct {
		name   string
		saved  time.Time
		minUTC int // 00:30 on the save date
		now    time.Time
		want   time.Time
	}{
		{"autumn", utc(2026, 10, 12, 10, 0), 22*60 + 30, utc(2026, 10, 24, 12, 0), utc(2026, 10, 25, 23, 30)},
		{"spring", utc(2027, 2, 1, 10, 0), 23*60 + 30, utc(2027, 3, 27, 12, 0), utc(2027, 3, 28, 22, 30)},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := c.saved
			m := dstManager(t, &now)
			m.SetSchedule("garage", Schedule{SoC: 0.8, TimeOfDayMinUTC: c.minUTC, Recurring: true, Days: monday})
			now = c.now
			m.RollSchedules(now)
			st, _ := m.State("garage")
			if !st.TargetTime.Equal(c.want) {
				t.Fatalf("deadline = %v, want %v", st.TargetTime, c.want)
			}
			local := st.TargetTime.In(loc)
			if local.Weekday() != time.Monday || local.Format("15:04") != "00:30" {
				t.Fatalf("deadline is %s %s on the household clock, want Monday 00:30", local.Weekday(), local.Format("15:04"))
			}
		})
	}
}

// 02:30 never shows on 2027-03-28 and shows twice on 2026-10-25. The car
// is due when the clock first reaches it, and only once that day.
func TestReadyTimeInSkippedOrRepeatedHour(t *testing.T) {
	loc := stockholm(t)
	s := Schedule{SoC: 0.8, Recurring: true, clock: 2*60 + 30, clockSet: true}
	for _, c := range []struct {
		name     string
		now      time.Time
		want     time.Time
		wantWall string
	}{
		{"skipped: the clock jumps from 02:00 to 03:00", utc(2027, 3, 27, 20, 0), utc(2027, 3, 28, 1, 0), "03:00"},
		{"repeated: the first 02:30", utc(2026, 10, 24, 20, 0), utc(2026, 10, 25, 0, 30), "02:30"},
		{"repeated: not again an hour later", utc(2026, 10, 25, 0, 45), utc(2026, 10, 26, 1, 30), "02:30"},
	} {
		got := s.NextDeadlineUTC(c.now, loc)
		if !got.Equal(c.want) || got.In(loc).Format("15:04") != c.wantWall {
			t.Errorf("%s: got %v (%s), want %v (%s)", c.name, got, got.In(loc).Format("15:04"), c.want, c.wantWall)
		}
	}
}

// A row saved before Core kept the household clock holds only a UTC
// minute. It is read on the boot date and saved back: before
// 2026-10-25 the summer time the owner set survives, after it the time
// the owner has seen since the change does.
func TestHydrateReadsOldRowOnBootDateAndSavesIt(t *testing.T) {
	loc := stockholm(t)
	const row = `{"soc":0.8,"time_of_day_min_utc":300,"recurring":true}` // 07:00 CEST
	for _, c := range []struct {
		name string
		boot time.Time
		want string
	}{
		{"booted before the change", utc(2026, 10, 12, 10, 0), "07:00"},
		{"booted after the change", utc(2026, 11, 2, 10, 0), "06:00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := c.boot
			m := dstManager(t, &now)
			var stored []byte
			m.SetScheduleSaver(func(_ string, s Schedule) (err error) {
				stored, err = s.MarshalStored()
				return err
			})
			err := m.HydrateSchedules(func(string) (Schedule, bool) {
				s, err := UnmarshalStored([]byte(row))
				return s, err == nil
			})
			if err != nil || stored == nil {
				t.Fatalf("old row not saved back: err=%v", err)
			}

			// A restart in the next season reads the saved clock, not UTC.
			now = utc(2027, 4, 1, 10, 0)
			again := dstManager(t, &now)
			again.SetScheduleSaver(func(string, Schedule) error {
				t.Fatal("a row with the household clock was saved again")
				return nil
			})
			if err := again.HydrateSchedules(func(string) (Schedule, bool) {
				s, err := UnmarshalStored(stored)
				return s, err == nil
			}); err != nil {
				t.Fatal(err)
			}
			again.RollSchedules(now)
			st, _ := again.State("garage")
			if got := st.TargetTime.In(loc).Format("15:04"); got != c.want {
				t.Fatalf("deadline %v is %s on the household clock, want %s", st.TargetTime, got, c.want)
			}
		})
	}
}

// state.db keeps the household clock. The API answer has the same fields
// as before, and a rollback to a Core without the clock still finds the
// UTC minute.
func TestStoredFormKeepsClockAndAPIFormDoesNot(t *testing.T) {
	s := Schedule{SoC: 0.8, TimeOfDayMinUTC: 300, Recurring: true, Days: 31, clock: 420, clockSet: true}
	raw, err := s.MarshalStored()
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalStored(raw)
	if err != nil || back != s {
		t.Fatalf("stored round trip: got %+v, %v; want %+v from %s", back, err, s, raw)
	}
	var old struct {
		TimeOfDayMinUTC int `json:"time_of_day_min_utc"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || old.TimeOfDayMinUTC != 300 {
		t.Fatalf("a rollback reads time_of_day_min_utc = %d from %s", old.TimeOfDayMinUTC, raw)
	}

	wire, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["time_of_day_min"]; ok {
		t.Fatalf("API form carries the stored clock: %s", wire)
	}
	if fields["time_of_day_min_utc"] != 300.0 {
		t.Fatalf("API form lost time_of_day_min_utc: %s", wire)
	}
}
