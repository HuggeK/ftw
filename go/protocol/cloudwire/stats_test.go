package cloudwire

import (
	"bytes"
	"testing"
	"time"
)

func TestStatisticsOnlyClosedAllowlistedQuantizedHours(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC).UnixMilli()
	valid := func() Stats {
		return Stats{StatsID: bytes.Repeat([]byte{1}, 16), Policy: StatsPolicy, HourMS: now/StatsHourMS*StatsHourMS - StatsHourMS, Metrics: map[string]int64{"pv_generation_wh": 120}}
	}
	if !valid().Valid(now) {
		t.Fatal("valid closed hour rejected")
	}
	cases := map[string]func(*Stats){"open hour": func(s *Stats) { s.HourMS += StatsHourMS }, "before window": func(s *Stats) { s.HourMS -= 24 * StatsHourMS }, "fine time": func(s *Stats) { s.HourMS++ }, "new field": func(s *Stats) { s.Metrics["address"] = 0 }, "fine energy": func(s *Stats) { s.Metrics["pv_generation_wh"] = 121 }, "too large": func(s *Stats) { s.Metrics["pv_generation_wh"] = 1000010 }, "old policy": func(s *Stats) { s.Policy = "old" }, "no id": func(s *Stats) { s.StatsID = nil }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid()
			change(&s)
			if s.Valid(now) {
				t.Fatal("invalid report accepted")
			}
		})
	}
	if _, ok := ClassScope[MsgStats]; ok {
		t.Fatal("statistics forwarded to clients")
	}
}
