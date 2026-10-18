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
