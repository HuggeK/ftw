package loadpoint

import (
	"context"
	"log/slog"
	"math"
	"time"
)

type resumeOffer struct {
	driver, device, session   string
	generation                uint64
	nextAttempt               time.Time
	retryDelay                time.Duration
	acknowledgedAttempts      int
	deliverySince, deliveryAt time.Time
	zeroOffer                 bool
	noDrawSince, noDrawAt     time.Time
}

// Allow a missed five-second tick and ordinary cloud poll jitter. Repeating
// one timestamp still cannot advance either delivery or no-draw proof.
const resumeObservationGap = 15 * time.Second

// recoverOfferedCharge resumes our zero offer, or a known plug session that
// still asks for charge after two minutes of accepted positive offers without
// delivery. The current limit must succeed before this optional resume. Only
// measured delivery ends recovery; an acknowledgement is not a charge.
func (c *Controller) recoverOfferedCharge(ctx context.Context, cfg Config, sample EVSample, offerW float64, now time.Time) {
	if c.resumeOffers == nil {
		c.resumeOffers = make(map[string]resumeOffer)
	}
	proof := resumeOffer{driver: cfg.DriverName, device: sample.DeviceID, session: sample.SessionID, generation: sample.ConnectionGeneration}
	previous, exists := c.resumeOffers[cfg.ID]
	if previous.driver != proof.driver || previous.device != proof.device || previous.session != proof.session || previous.generation != proof.generation {
		delete(c.resumeOffers, cfg.ID)
		exists = false
	}
	if offerW <= 0 {
		proof.zeroOffer = true
		c.resumeOffers[cfg.ID] = proof
		return
	}
	if !exists {
		if proof.device == "" || proof.session == "" || !sample.RequestActive {
			return
		}
		previous = proof
		previous.noDrawSince = now
	}
	if !previous.zeroOffer && !sample.RequestActive {
		delete(c.resumeOffers, cfg.ID)
		return
	}
	// A small startup pulse is not successful recovery. Require fresh,
	// advancing observations near a supported charging step for ten seconds.
	// The current offer can be lower than the configured 3p minimum after a
	// phase change. Leave the shared DeliveringW semantics unchanged.
	minimum := cfg.MinChargeW
	if minimum <= 0 {
		minimum = offerW
	}
	// Allow ten percent variation around the nominal-voltage minimum.
	floor := math.Max(DeliveringW, 0.9*math.Min(minimum, offerW))
	observedAt := sample.PowerAt
	if observedAt.IsZero() {
		observedAt = now // Legacy telemetry uses the fresh-read contract.
	}
	fresh := !sample.PowerUnavailable && !observedAt.After(now) && now.Sub(observedAt) <= min(sample.PowerWindow(), resumeObservationGap)
	if !fresh || !finite(sample.PowerW) {
		previous.deliverySince, previous.deliveryAt = time.Time{}, time.Time{}
		previous.noDrawSince, previous.noDrawAt = time.Time{}, time.Time{}
		c.resumeOffers[cfg.ID] = previous
		return
	}
	if sample.PowerW >= floor {
		if previous.deliverySince.IsZero() || observedAt.Before(previous.deliveryAt) || observedAt.Sub(previous.deliveryAt) > resumeObservationGap {
			previous.deliverySince = observedAt
		}
		previous.deliveryAt = observedAt
		if observedAt.Sub(previous.deliverySince) >= 10*time.Second {
			delete(c.resumeOffers, cfg.ID)
		} else {
			c.resumeOffers[cfg.ID] = previous
		}
		return // Already drawing: observe it rather than sending another resume.
	}
	previous.deliverySince, previous.deliveryAt = time.Time{}, time.Time{}
	if !previous.zeroOffer {
		if previous.noDrawSince.IsZero() || observedAt.Before(previous.noDrawAt) ||
			(!previous.noDrawAt.IsZero() && observedAt.Sub(previous.noDrawAt) > resumeObservationGap) {
			previous.noDrawSince = now
		}
		previous.noDrawAt = observedAt
		c.resumeOffers[cfg.ID] = previous
		if previous.noDrawAt.Sub(previous.noDrawSince) < 2*time.Minute {
			return
		}
	}
	c.resumeOffers[cfg.ID] = previous
	if now.Before(previous.nextAttempt) {
		return
	}
	if previous.acknowledgedAttempts >= 3 {
		if previous.acknowledgedAttempts == 3 {
			slog.Warn("loadpoint resume acknowledged but no charge observed", "lp", cfg.ID, "driver", cfg.DriverName)
			previous.acknowledgedAttempts++
			c.resumeOffers[cfg.ID] = previous
		}
		return
	}
	if err := c.sendDispatchWithDeadline(ctx, cfg.DriverName, []byte(`{"action":"ev_resume"}`)); err != nil {
		previous.retryDelay = min(time.Minute, max(5*time.Second, 2*previous.retryDelay))
		previous.nextAttempt = now.Add(previous.retryDelay)
		c.resumeOffers[cfg.ID] = previous
		slog.Warn("loadpoint resume failed", "lp", cfg.ID, "driver", cfg.DriverName,
			"retry_at", previous.nextAttempt, "err", err)
		return
	}
	// An HTTP acknowledgement does not establish that the contactor closed.
	// Retry at a bounded rate, under the current dispatch gates, and stop after
	// three acknowledged attempts. Only observed delivery clears the proof.
	previous.acknowledgedAttempts++
	slog.Info("loadpoint resume acknowledged; waiting for measured charge", "lp", cfg.ID, "driver", cfg.DriverName,
		"attempt", previous.acknowledgedAttempts, "offer_w", offerW, "observed_w", sample.PowerW, "power_at", sample.PowerAt)
	previous.nextAttempt = now.Add(30 * time.Second)
	previous.retryDelay = 0
	c.resumeOffers[cfg.ID] = previous
}
