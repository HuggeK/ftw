package telemetry

import (
	"slices"
	"testing"
)

func TestOfflineDriversNamesOnlySilentDriversOfOneKind(t *testing.T) {
	s := NewStore()
	s.Update("live", DerPV, -1200, nil, nil)
	s.RecordDriverSuccess("live")
	s.Update("asleep", DerPV, 0, nil, nil)
	s.RecordDriverSuccess("asleep")
	s.DriverHealthMut("asleep").SetOffline()
	s.Update("new", DerPV, 0, nil, nil) // no health yet
	s.Update("battery", DerBattery, 0, nil, nil)
	s.DriverHealthMut("battery").SetOffline()
	if got := s.OfflineDrivers(DerPV); !slices.Equal(got, []string{"asleep", "new"}) {
		t.Fatalf("offline PV drivers = %v", got)
	}
}
