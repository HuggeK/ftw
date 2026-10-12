package main

import (
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestHistoryRetainsLiveMeterWhenBatteryOffline(t *testing.T) {
	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "battery"} {
		tel.EnsureDriverHealth(name)
	}
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")
	tel.DriverHealthMut("battery").SetOffline()
	opts := telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
	p, available := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "meter"}, time.Now().UnixMilli(), time.Minute, opts)
	if !available || p.GridW != 1200 {
		t.Fatalf("live meter lost when battery offline: available=%v point=%+v", available, p)
	}
}

func TestHistoryConfiguredPVWithoutMeasurementIsUnknown(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.RecordDriverSuccess("meter")
	p, ok := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "meter"}, time.Now().UnixMilli(), time.Minute, telemetry.ForecastOptions{HouseholdInvalidReason: "configured_pv_without_measurement_source"})
	if !ok || p.Missing&state.MissingPV == 0 {
		t.Fatalf("unmeasured configured PV reported as zero: %+v", p)
	}
}
