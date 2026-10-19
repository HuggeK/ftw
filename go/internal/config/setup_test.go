package config

import (
	"math"
	"testing"
)

func TestAnnualPriorAccounting(t *testing.T) {
	yes, no := true, false
	total, ev, heat := 20000.0, 4000.0, 8000.0
	c := Config{Weather: &Weather{HeatingWPerDegC: 250}, ForecastPriors: &ForecastPriors{
		AnnualKWh: &total, AnnualBasis: "total_consumption", IncludesEV: &yes, EVAnnualKWh: &ev,
		IncludesHeating: &yes, HeatingAnnualKWh: &heat,
	}}
	if kWh, why := c.AnnualBaseKWh(); kWh != 8000 || why != "" {
		t.Fatalf("base = %v, %q", kWh, why)
	}
	for _, change := range []func(*ForecastPriors){
		func(p *ForecastPriors) { p.AnnualBasis = "grid_import" },
		func(p *ForecastPriors) { p.IncludesEV = nil },
		func(p *ForecastPriors) { p.EVAnnualKWh = nil },
		func(p *ForecastPriors) { p.IncludesHeating = nil },
		func(p *ForecastPriors) { p.HeatingAnnualKWh = nil },
	} {
		copy := *c.ForecastPriors
		change(&copy)
		next := c
		next.ForecastPriors = &copy
		if kWh, why := next.AnnualBaseKWh(); kWh != 0 || why == "" {
			t.Fatalf("incomplete prior applied: %v, %q", kWh, why)
		}
	}
	c.ForecastPriors.IncludesEV, c.ForecastPriors.IncludesHeating = &no, &no
	if kWh, why := c.AnnualBaseKWh(); kWh != total || why != "" {
		t.Fatalf("excluded components subtracted: %v, %q", kWh, why)
	}
}

func TestSetupOptionalAndControlValidation(t *testing.T) {
	c := *NewSetup()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Setup.MeasurementOnly = false
	if err := c.Validate(); err == nil {
		t.Fatal("enabled control without a confirmed connection")
	}
	c.Setup.ConnectionConfirmed, c.Setup.EquipmentReviewed = true, true
	c.Fuse = Fuse{MaxAmps: 20, Phases: 3, Voltage: 230}
	c.Drivers = []Driver{{Name: "meter", Lua: "drivers/meter.lua", IsSiteMeter: true, Capabilities: Capabilities{Standalone: true}}}
	c.Setup.ConnectionDigest, c.Setup.MeterIdentity = c.SetupConnectionDigest(), "serial:test-meter"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	nan := math.NaN()
	c.ForecastPriors = &ForecastPriors{AnnualKWh: &nan}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted nonfinite prior")
	}
}
