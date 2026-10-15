//go:build !linux

package cloudlink

import "time"

// ClockSynced has no kernel to ask outside Linux, where boxes run. A
// development machine keeps its own time, so a clock past 2025 is taken as
// set. A wrong answer only makes data look older on the phone: rule N takes
// the earlier of the box's wall time and the arrival, and drops a box clock
// that runs ahead.
func ClockSynced() bool {
	return time.Now().Year() >= 2025
}
