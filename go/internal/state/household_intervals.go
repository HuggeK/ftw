package state

import (
	"context"
	"fmt"
)

// HouseholdIntervalQuality labels training evidence read from these intervals.
const HouseholdIntervalQuality = "source_interval_balance_v1"

// HouseholdInterval is the mean household power over one closed interval,
// balanced from the same source energy as daily Home use. Load models train
// on these, so a forecast learns the Home use that History shows.
type HouseholdInterval struct {
	StartMS, EndMS int64
	LoadW          float64
}

// Training is strict where display is lenient: History may show Home use
// across a bridged or estimated gap, but a model learns only from intervals
// measured in full.
const householdTrainingExcluded = MissingPower | BalanceMismatch | EstimatedLoad

// HouseholdIntervals returns closed, fully measured intervals in [from, to)
// and the end of the closed range, so a caller can move past intervals left
// out. from must be aligned to width. An interval is closed once committed
// source progress reaches its end; later writes cannot change it. Intervals
// with a missing source, a recording gap, an estimate or a negative balance
// are left out.
func (s *Store) HouseholdIntervals(ctx context.Context, from, to, width int64) ([]HouseholdInterval, int64, error) {
	if width <= 0 || from%width != 0 {
		return nil, from, fmt.Errorf("household intervals: from %d is not aligned to %d ms", from, width)
	}
	through, ok, err := s.HistorySourceProgress(ctx)
	if err != nil || !ok {
		return nil, from, err
	}
	to = bucketStart(min(to, through), width)
	if to <= from {
		return nil, from, nil
	}
	buckets, err := s.sourceLoadBuckets(ctx, from, to, width)
	if err != nil {
		return nil, from, err
	}
	var out []HouseholdInterval
	for _, b := range buckets {
		if b.width != width || b.covered != width || b.missing&householdTrainingExcluded != 0 || b.wh < 0 || !finiteHistoryValue(b.wh) {
			continue
		}
		out = append(out, HouseholdInterval{StartMS: b.start, EndMS: b.start + width, LoadW: b.wh * 3600000 / float64(width)})
	}
	return out, to, nil
}
