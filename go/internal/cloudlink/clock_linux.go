package cloudlink

import "golang.org/x/sys/unix"

// ClockSynced asks the kernel whether NTP has synchronised the clock, the
// same question timedatectl answers. A box without an RTC reads 1970 until
// it has, and a wall time from then must not anchor any age.
func ClockSynced() bool {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil || state == unix.TIME_ERROR {
		return false
	}
	// systemd's rule: synchronised, with an error bound under 16 s.
	return tx.Status&unix.STA_UNSYNC == 0 && tx.Maxerror < 16_000_000
}
