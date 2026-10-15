package telemetry

import (
	"math"
	"testing"
	"time"
	"unsafe"
)

// A clock correction changes wall time while the monotonic clock keeps running.
// The public time API cannot construct that pair. Keep this test-only layout
// guarded by size and clock checks, and never change the host clock.
func forecastTestWallJump(t *testing.T, at time.Time, jump time.Duration) time.Time {
	t.Helper()
	type timeLayout struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	if unsafe.Sizeof(at) != unsafe.Sizeof(timeLayout{}) {
		t.Fatal("time.Time layout changed; update the clock-jump fixture")
	}
	before := at
	clock := (*timeLayout)(unsafe.Pointer(&at))
	if clock.wall&(1<<63) == 0 || jump%time.Second != 0 {
		t.Fatal("clock-jump fixture needs monotonic time and whole seconds")
	}
	clock.wall += uint64(int64(jump/time.Second) << 30)
	if at.Sub(before) != 0 || at.Round(0).Sub(before.Round(0)) != jump {
		t.Fatal("clock-jump fixture did not preserve monotonic time")
	}
	return at
}

func TestForecastAccumulatorWallClockJumpDoesNotFillMissingQuarters(t *testing.T) {
	for _, jump := range []time.Duration{110 * time.Minute, -110 * time.Minute} {
		t.Run(jump.String(), func(t *testing.T) {
			start := time.Now()
			start = start.Add(-time.Duration(start.Minute()%15)*time.Minute - time.Duration(start.Second())*time.Second - time.Duration(start.Nanosecond()))
			a := ForecastAccumulator{}
			read := func(at time.Time, load float64) []ForecastInterval {
				return a.Observe(ForecastReading{At: at, Latest: at, Valid: true, PVValid: true, HouseholdW: load, PVW: -500})
			}
			read(start, 1000)
			read(start.Add(10*time.Second), 1000)
			after := forecastTestWallJump(t, start.Add(20*time.Second), jump)
			if got := read(after, 1600); len(got) != 0 {
				t.Fatalf("clock jump invented complete measurements across a gap: %+v", got)
			}
			var got []ForecastInterval
			for elapsed := 10 * time.Second; elapsed <= 31*time.Minute; elapsed += 10 * time.Second {
				got = append(got, read(after.Add(elapsed), 1600)...)
			}
			if len(got) == 0 {
				t.Fatal("fresh measurements did not recover after the clock jump")
			}
			for _, interval := range got {
				if interval.Start.Before(after.Round(0)) || math.Abs(interval.HouseholdW-1600) > 1e-9 || math.Abs(interval.PVW+500) > 1e-9 {
					t.Fatalf("recovered interval contains missing or extrapolated data: %+v", interval)
				}
			}
		})
	}
}
