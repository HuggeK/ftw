package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The fuse guard raises the offer by 1 A. Each material step must not buy
// another two minutes of Adjusting when the charger keeps confirming zero.
func TestEVRampingOfferDoesNotHideSustainedZeroDraw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		srv := New(&Deps{Tel: tel})
		for i := 0; i <= 25; i++ {
			now := time.Now()
			power := 4140 + 690*((i/5)%10)
			cmd := tel.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%d}`, power)), now)
			tel.CompleteCommand(cmd, "accepted")
			tel.Update("easee", telemetry.DerEV, 0, nil, []byte(`{"max_a":15}`))
			if i == 25 {
				f := srv.controlFeedback(time.Now())[0]
				if f.Reason != "no_power_response" || f.Status != "not_following" {
					t.Fatalf("zero draw hidden by a new positive offer: %+v", f)
				}
			}
			time.Sleep(5 * time.Second)
		}
	})
}

func TestEVNoPowerWarningRequiresAdvancingObservations(t *testing.T) {
	for _, core := range []string{"fuse_limit", "manual_hold"} {
		for _, advancing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/advancing=%t", core, advancing), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					tel := telemetry.NewStore()
					srv := New(&Deps{Tel: tel})
					cmd := tel.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":6900}`), start)
					tel.CompleteCommand(cmd, "accepted")
					for i := 0; i <= 25; i++ {
						at := start
						if advancing {
							at = time.Now()
						}
						tel.Update("easee", telemetry.DerEV, 0, nil, []byte(fmt.Sprintf(
							`{"max_a":10,"power_max_age_s":180,"control_power_observed_at":%q}`, at.Format(time.RFC3339Nano))))
						if i < 25 {
							time.Sleep(5 * time.Second)
						}
					}
					f := srv.controlFeedback(time.Now())[0]
					evidence, _ := tel.CommandEvidence("easee", "ev")
					connected := true
					classifyControlFeedback(&f, evidence, true, true, core, &connected, time.Now())
					want := "no_power_response"
					if !advancing {
						want = "waiting_response"
						if core == "fuse_limit" {
							want = core
						}
					}
					if f.Reason != want {
						t.Fatalf("got %s, want %s; evidence: %+v", f.Reason, want, evidence)
					}
				})
			})
		}
	}
}

func TestEVStartupPulseDoesNotHideAnObservedMismatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := telemetry.NewStore()
		srv := New(&Deps{Tel: tel})
		cmd := tel.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":6900}`), time.Now())
		tel.CompleteCommand(cmd, "accepted")
		for i := 0; i <= 25; i++ {
			power := 0.0
			if i == 20 {
				power = 299
			}
			tel.Update("easee", telemetry.DerEV, power, nil, []byte(`{"max_a":10}`))
			if i < 25 {
				time.Sleep(5 * time.Second)
			}
		}
		f := srv.controlFeedback(time.Now())[0]
		evidence, _ := tel.CommandEvidence("easee", "ev")
		connected := true
		classifyControlFeedback(&f, evidence, true, true, "fuse_limit", &connected, time.Now())
		if f.Reason != "no_power_response" {
			t.Fatalf("startup pulse hid a two-minute measured shortfall: %+v", f)
		}
	})
}
