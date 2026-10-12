package loadpoint

import (
	"os"
	"testing"
	"time"
)

// Tests read the household clock in UTC unless they pin a zone with
// SetLocation. A client's UTC minute is read on the date it is saved,
// so the machine's zone and season would otherwise move deadlines that
// tests compare with fixed dates.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
