package telemetry

import (
	"fmt"
	"testing"
	"time"
)

func TestEVNoPowerEvidenceEndsAtControlAndMeasurementBoundaries(t *testing.T) {
	for _, boundary := range []string{"pause", "failed", "unknown power", "drawing", "gap", "released"} {
		t.Run(boundary, func(t *testing.T) {
			s := NewStore()
			start := time.Now()
			command := func(at time.Time, power float64, result string) {
				c := s.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, power)), at)
				s.CompleteCommand(c, result)
			}
			observe := func(at time.Time, power float64, data string) {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.observeCommand("easee", DerEV, power, []byte(data), at)
			}
			for i := 0; i <= 25; i++ {
				at := start.Add(time.Duration(i) * 5 * time.Second)
				command(at, 4140+690*float64((i/5)%10), "accepted")
				observe(at, 0, `{}`)
			}
			old, _ := s.CommandEvidence("easee", "ev")
			if old.NoPowerAt.Sub(old.NoPowerSince) != 125*time.Second {
				t.Fatalf("retuning lost the zero-power interval: %+v", old)
			}
			at := start.Add(130 * time.Second)
			switch boundary {
			case "pause":
				command(at, 0, "accepted")
			case "failed":
				command(at, 10350, "failed")
			case "unknown power":
				observe(at, 0, `{"control_power_available":false}`)
			case "drawing":
				observe(at, 4712, `{}`)
			case "gap":
				at = at.Add(2 * time.Minute)
			case "released":
				s.EndCommandControl("easee", false)
			}
			command(at.Add(time.Second), 10350, "accepted")
			observe(at.Add(time.Second), 0, `{}`)
			c, _ := s.CommandEvidence("easee", "ev")
			if !c.NoPowerSince.Equal(at.Add(time.Second)) {
				t.Fatalf("zero proof crossed %s: %+v", boundary, c)
			}
		})
	}
}

func TestResumeDoesNotReplaceAcceptedCurrentEvidence(t *testing.T) {
	s := NewStore()
	now := time.Now()
	c := s.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":4140}`), now)
	s.CompleteCommand(c, "accepted")
	resume := s.BeginCommand("easee", []byte(`{"action":"ev_resume"}`), now.Add(time.Second))
	s.CompleteCommand(resume, "accepted")
	got, ok := s.CommandEvidence("easee", "ev")
	if !ok || got.PowerW == nil || *got.PowerW != 4140 || !got.At.Equal(now) || got.Result != "accepted" {
		t.Fatalf("optional resume replaced the current limit: %+v", got)
	}
}
