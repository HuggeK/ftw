package loadpoint

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// A vehicle API can still say Stopped while its charger measures a charge.
// A wake must never turn that delayed vehicle report into a charger pause.
func TestAutomaticWakeRequiresFreshZeroChargerPower(t *testing.T) {
	for _, tc := range []struct {
		name        string
		power       float64
		unavailable bool
		age         time.Duration
	}{
		{"charging", 4712, false, 0},
		{"startup pulse", 299, false, 0},
		{"missing power", 0, true, 0},
		{"old zero", 0, false, 4 * time.Minute},
		{"future zero", 0, false, -time.Minute},
		{"invalid power", math.NaN(), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				sender := &outcomeSender{}
				c, _, _ := outcomeFixture(t, now, sender)
				c.Tick(context.Background(), now)
				c.tel = func(string) (EVSample, bool) {
					return EVSample{Connected: true, RequestActive: true, PowerW: tc.power, PowerUnavailable: tc.unavailable, PowerAt: now.Add(time.Second).Add(-tc.age)}, true
				}
				c.SetVehicleStatus(func(string) (string, string, bool) { return "vehicle", "Stopped", true })
				c.Tick(context.Background(), now.Add(time.Second))
				time.Sleep(wallboxCycleGap)
				synctest.Wait()
				for _, call := range sender.sent() {
					if call.action == "charge_start" || call.action == "ev_pause" || call.action == "ev_resume" {
						t.Fatalf("automatic restart from %s: %+v", tc.name, call)
					}
				}
			})
		})
	}
}

func TestReadOnlyVehicleDoesNotArmWakeOrCycleCharger(t *testing.T) {
	now := time.Now()
	sender := &outcomeSender{}
	c, cfg, _ := outcomeFixture(t, now, sender)
	c.SetVehicleStatus(func(string) (string, string, bool) { return "vag_vehicle", "Stopped", true })
	c.SetVehicleControlAllowed(func(string) bool { return false })
	c.Tick(context.Background(), now)
	c.wakeVehicleAuto(context.Background(), cfg.ID, "delivering_edge")
	if _, err := c.ForceStartVehicle(context.Background(), cfg.ID); err != ErrForceStartNoVehicleBound {
		t.Fatalf("read-only force start: %v", err)
	}
	for _, call := range sender.sent() {
		if call.action != "ev_set_current" || call.driver != cfg.DriverName {
			t.Fatalf("read-only vehicle caused a wake or contactor cycle: %+v", call)
		}
	}
	if len(c.wakeLast) != 0 || len(c.wakeKickUntil) != 0 || len(c.wakeAttempts) != 0 {
		t.Fatal("read-only vehicle armed a wake kick or backoff")
	}
}

func TestAcceptedVehicleStartDoesNotPauseCharger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sender := &outcomeSender{}
		c, _, _ := outcomeFixture(t, time.Now(), sender)
		c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
		c.Tick(context.Background(), time.Now())
		time.Sleep(wallboxCycleGap)
		synctest.Wait()
		starts := 0
		for _, call := range sender.sent() {
			if call.action == "charge_start" {
				starts++
			}
			if call.action == "ev_pause" {
				t.Fatal("accepted vehicle start was followed by a charger pause")
			}
		}
		if starts != 1 {
			t.Fatalf("got %d vehicle starts", starts)
		}
	})
}

func TestWakeRechecksAfterVehicleStartRefusal(t *testing.T) {
	for _, boundary := range []string{"stop", "unplug", "session", "unknown", "charging", "missing power"} {
		t.Run(boundary, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				sender := &outcomeSender{errFor: map[string]error{"tesla": errors.New("vehicle refused start")}}
				c, cfg, _ := outcomeFixture(t, now, sender)
				sample := EVSample{Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "one", PowerAt: now}
				c.tel = func(string) (EVSample, bool) { return sample, true }
				c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
				c.send = func(ctx context.Context, driver string, payload []byte) error {
					if driver == "tesla" {
						switch boundary {
						case "stop":
							c.SetManualHold(cfg.ID, ManualHold{PowerW: 0, Persistent: true})
						case "unplug":
							sample.Connected = false
						case "session":
							sample.SessionID = "two"
						case "unknown":
							sample.ConnectionUnknown = true
						case "charging":
							sample.PowerW = 4712
						case "missing power":
							sample.PowerUnavailable = true
						}
					}
					return sender.Send(ctx, driver, payload)
				}
				c.Tick(context.Background(), now)
				time.Sleep(wallboxCycleGap)
				synctest.Wait()
				for _, call := range sender.sent() {
					if call.action == "ev_pause" || call.action == "ev_resume" {
						t.Fatalf("charger cycle crossed %s during vehicle start: %+v", boundary, call)
					}
				}
			})
		})
	}
}
