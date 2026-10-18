package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestPositiveOfferRecoveryWithoutPriorZero(t *testing.T) {
	for _, boundary := range []string{"none", "poll jitter", "charging", "cached zero", "declined", "unknown identity", "unknown power", "safety", "manual stop", "session changed", "current refused", "unplug after retry", "unknown after retry", "declined after retry", "safety after retry"} {
		t.Run(boundary, func(t *testing.T) {
			now := time.Now()
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 10350}
			dir := &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 10350}}
			sender := &fakeSender{}
			sample := EVSample{Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "plug-1", ConnectionGeneration: 1}
			samples := map[string]EVSample{cfg.DriverName: sample}
			c := newTestController(t, []Config{cfg}, dir, samples, sender)
			if boundary == "current refused" {
				c.send = func(ctx context.Context, driver string, payload []byte) error {
					if string(payload) == `{"action":"ev_resume"}` {
						return sender.Send(ctx, driver, payload)
					}
					return errors.New("current refused")
				}
			}
			for seconds := 0; seconds <= 210; seconds += 5 {
				at := now.Add(time.Duration(seconds) * time.Second)
				sample.PowerAt = at
				switch boundary {
				case "poll jitter":
					sample.PowerAt = now.Add(time.Duration(math.Floor(float64(seconds)/10.1) * 10.1 * float64(time.Second)))
				case "charging":
					sample.PowerW = 4712
				case "cached zero":
					sample.PowerAt = now
				case "declined":
					sample.RequestActive = false
				case "unknown identity":
					sample.SessionID = ""
				case "unknown power":
					sample.PowerUnavailable = true
				case "session changed":
					if seconds == 100 {
						sample.SessionID = "plug-2"
						sample.ConnectionGeneration++
					}
				case "manual stop":
					if seconds == 100 {
						c.SetManualHold(cfg.ID, ManualHold{PowerW: 0, StartedAt: at, Persistent: true})
					}
				case "unplug after retry":
					if seconds >= 125 {
						sample.Connected = false
					}
				case "unknown after retry":
					if seconds >= 125 {
						sample.ConnectionUnknown = true
					}
				case "declined after retry":
					if seconds >= 125 {
						sample.RequestActive = false
					}
				}
				samples[cfg.DriverName] = sample
				c.TickWithDispatch(context.Background(), at, boundary != "safety" && !(boundary == "safety after retry" && seconds >= 125))
			}
			want := 0
			if boundary == "none" || boundary == "poll jitter" {
				want = 3
			}
			if boundary == "unplug after retry" || boundary == "unknown after retry" || boundary == "declined after retry" || boundary == "safety after retry" {
				want = 1
			}
			if got := countAction(sender.calls, "ev_resume"); got != want {
				t.Fatalf("%s: got %d resumes, want %d", boundary, got, want)
			}
			if countAction(sender.calls, "ev_pause") != 0 {
				t.Fatal("recovery must not pause the charger")
			}
		})
	}
}

func TestRecoveryRechecksStopAndSessionAfterCurrentWrite(t *testing.T) {
	for _, boundary := range []string{"stop", "unplug", "session", "unknown", "started drawing", "stop during resume", "drawing during resume"} {
		t.Run(boundary, func(t *testing.T) {
			now := time.Now()
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 10350}
			dir := &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 0}}
			sample := EVSample{Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "one", ConnectionGeneration: 1}
			samples := map[string]EVSample{cfg.DriverName: sample}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, dir, samples, sender)
			c.Tick(context.Background(), now)
			c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
			dir.LoadpointEnergyWh[cfg.ID] = 10350
			c.send = func(ctx context.Context, driver string, payload []byte) error {
				var command struct {
					Action string  `json:"action"`
					PowerW float64 `json:"power_w"`
				}
				if err := json.Unmarshal(payload, &command); err != nil {
					return err
				}
				if command.Action == "ev_resume" {
					switch boundary {
					case "stop during resume":
						c.SetManualHold(cfg.ID, ManualHold{PowerW: 0, Persistent: true})
					case "drawing during resume":
						sample.PowerW = 4712
					}
					samples[cfg.DriverName] = sample
				}
				if command.Action == "ev_set_current" && command.PowerW > 0 {
					switch boundary {
					case "stop":
						c.SetManualHold(cfg.ID, ManualHold{PowerW: 0, Persistent: true})
					case "unplug":
						sample.Connected = false
					case "session":
						sample.SessionID = "two"
					case "unknown":
						sample.ConnectionUnknown = true
					case "started drawing":
						sample.PowerW = 4712
					}
					samples[cfg.DriverName] = sample
				}
				return sender.Send(ctx, driver, payload)
			}
			c.Tick(context.Background(), now.Add(5*time.Second))
			wantResumes := 0
			if boundary == "stop during resume" || boundary == "drawing during resume" {
				wantResumes = 1
			}
			if countAction(sender.calls, "ev_resume") != wantResumes {
				t.Fatalf("resume crossed %s during current write", boundary)
			}
			if countAction(sender.calls, "charge_start") != 0 || countAction(sender.calls, "ev_pause") != 0 {
				t.Fatalf("vehicle wake crossed %s during current write", boundary)
			}
		})
	}
}
