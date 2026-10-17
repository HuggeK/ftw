package appuplink

import (
	"context"
	"time"
)

// runRelay is kept only for the old protocol's regression fixtures.
func (u *Uplink) runRelay(ctx context.Context) error {
	for ctx.Err() == nil {
		delay, _ := u.once(ctx)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
