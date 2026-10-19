package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Setup keeps first-install choices with the settings the owner reviews.
// Existing installations without this section keep their current behaviour.
// Confirmation records what the owner checked; it is not hardware evidence.
type Setup struct {
	GoalsReviewed       bool   `yaml:"goals_reviewed,omitempty" json:"goals_reviewed,omitempty"`
	MeasurementOnly     bool   `yaml:"measurement_only" json:"measurement_only"`
	ConnectionConfirmed bool   `yaml:"connection_confirmed" json:"connection_confirmed"`
	EquipmentReviewed   bool   `yaml:"equipment_reviewed" json:"equipment_reviewed"`
	PriceReviewed       bool   `yaml:"price_reviewed" json:"price_reviewed"`
	SolarPresent        *bool  `yaml:"solar_present,omitempty" json:"solar_present,omitempty"`
	ConnectionDigest    string `yaml:"connection_digest,omitempty" json:"connection_digest,omitempty"`
	MeterIdentity       string `yaml:"meter_identity,omitempty" json:"meter_identity,omitempty"`
}

// NewSetup starts local services with no equipment or assumed fuse rating.
// Enabling the local connection service does not accept Cloud terms.
func NewSetup() *Config {
	c := &Config{
		Setup:     &Setup{MeasurementOnly: true},
		AppLink:   &AppLink{Enabled: true},
		FleetPing: &FleetPing{Enabled: false},
		Drivers:   []Driver{},
	}
	applyDefaults(c)
	return c
}

// ForecastPriors records optional owner estimates, not measured history.
// AnnualKWh is a full year's electricity use. Grid purchases alone cannot
// establish household demand when solar or storage also supplies the home.
type ForecastPriors struct {
	Reviewed         bool     `yaml:"reviewed,omitempty" json:"reviewed,omitempty"`
	AnnualKWh        *float64 `yaml:"annual_kwh,omitempty" json:"annual_kwh,omitempty"`
	AnnualBasis      string   `yaml:"annual_basis,omitempty" json:"annual_basis,omitempty"` // total_consumption, grid_import, unknown
	AnnualPeriod     string   `yaml:"annual_period,omitempty" json:"annual_period,omitempty"`
	IncludesEV       *bool    `yaml:"includes_ev,omitempty" json:"includes_ev,omitempty"`
	EVAnnualKWh      *float64 `yaml:"ev_annual_kwh,omitempty" json:"ev_annual_kwh,omitempty"`
	IncludesHeating  *bool    `yaml:"includes_heating,omitempty" json:"includes_heating,omitempty"`
	HeatingAnnualKWh *float64 `yaml:"heating_annual_kwh,omitempty" json:"heating_annual_kwh,omitempty"`
	HeatingType      string   `yaml:"heating_type,omitempty" json:"heating_type,omitempty"`
	Occupants        *int     `yaml:"occupants,omitempty" json:"occupants,omitempty"`
	Source           string   `yaml:"source,omitempty" json:"source,omitempty"`
	RecordedAt       string   `yaml:"recorded_at,omitempty" json:"recorded_at,omitempty"` // RFC3339
	SolarSource      string   `yaml:"solar_source,omitempty" json:"solar_source,omitempty"`
	SolarRecordedAt  string   `yaml:"solar_recorded_at,omitempty" json:"solar_recorded_at,omitempty"`
}

// AnnualBaseKWh returns an estimate for the load model's non-EV baseline.
// A separately modelled heating term must also be removed from the annual
// total. Missing answers leave the current generic prior in place.
func (c *Config) AnnualBaseKWh() (float64, string) {
	p := c.ForecastPriors
	if p == nil || p.AnnualKWh == nil {
		return 0, "annual_use_unknown"
	}
	if p.AnnualBasis != "total_consumption" {
		return 0, "total_consumption_needed"
	}
	if p.IncludesEV == nil {
		return 0, "ev_inclusion_unknown"
	}
	base := *p.AnnualKWh
	if *p.IncludesEV {
		if p.EVAnnualKWh == nil {
			return 0, "ev_use_unknown"
		}
		base -= *p.EVAnnualKWh
	}
	if c.Weather != nil && c.Weather.HeatingWPerDegC > 0 {
		if p.IncludesHeating == nil {
			return 0, "heating_inclusion_unknown"
		}
		if *p.IncludesHeating {
			if p.HeatingAnnualKWh == nil {
				return 0, "heating_use_unknown"
			}
			base -= *p.HeatingAnnualKWh
		}
	}
	if base <= 0 || math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, "annual_components_exceed_total"
	}
	return base, ""
}

// AnnualPriorIncludesHeating distinguishes a coarse whole-house prior from
// a baseline to which a separate weather-sensitive heating term is added.
func (c *Config) AnnualPriorIncludesHeating() bool {
	if c.ForecastPriors == nil || c.Weather != nil && c.Weather.HeatingWPerDegC > 0 {
		return false
	}
	return c.ForecastPriors.IncludesHeating == nil || *c.ForecastPriors.IncludesHeating
}

func (c *Config) MeasurementOnly() bool {
	return c.Setup != nil && c.Setup.MeasurementOnly
}

// SetupConnectionDigest binds the owner's confirmation to the physical
// electrical connection. The live device identity is a
// separate runtime check, never inferred from a display name.
func (c *Config) SetupConnectionDigest() string {
	data, _ := json.Marshal(struct {
		MaxAmps float64
		Phases  int
		Voltage float64
	}{c.Fuse.MaxAmps, c.Fuse.Phases, c.Fuse.Voltage})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (c *Config) validateSetup() error {
	if s := c.Setup; s != nil && !s.MeasurementOnly {
		if !s.ConnectionConfirmed || !(c.Fuse.MaxAmps > 0) || c.Fuse.Phases < 1 || c.Fuse.Phases > 3 || !(c.Fuse.Voltage > 0) || math.IsInf(c.Fuse.MaxAmps, 0) || math.IsInf(c.Fuse.Voltage, 0) {
			return fmt.Errorf("setup: confirm the main fuse, phases and voltage per phase before enabling control")
		}
		if !s.EquipmentReviewed {
			return fmt.Errorf("setup: review the selected equipment before enabling control")
		}
		if c.SiteMeterDriver() == "" || s.MeterIdentity == "" || s.ConnectionDigest != c.SetupConnectionDigest() {
			return fmt.Errorf("setup: confirm this electrical connection and the live site meter identity before enabling control")
		}
	}
	p := c.ForecastPriors
	if p == nil {
		return nil
	}
	for _, field := range []struct {
		name string
		v    *float64
	}{{"annual_kwh", p.AnnualKWh}, {"ev_annual_kwh", p.EVAnnualKWh}, {"heating_annual_kwh", p.HeatingAnnualKWh}} {
		if field.v != nil && (math.IsNaN(*field.v) || math.IsInf(*field.v, 0) || *field.v < 0 || *field.v > 10_000_000) {
			return fmt.Errorf("forecast_priors.%s must be a finite annual kWh value between 0 and 10000000", field.name)
		}
	}
	switch p.AnnualBasis {
	case "", "unknown", "total_consumption", "grid_import":
	default:
		return fmt.Errorf("forecast_priors.annual_basis must be total_consumption, grid_import or unknown")
	}
	if p.Occupants != nil && (*p.Occupants < 0 || *p.Occupants > 1000) {
		return fmt.Errorf("forecast_priors.occupants must be between 0 and 1000")
	}
	if len(p.Source) > 512 || len(p.SolarSource) > 512 || len(p.HeatingType) > 128 || len(p.AnnualPeriod) > 128 {
		return fmt.Errorf("forecast_priors source or heating_type is too long")
	}
	for _, recorded := range []string{p.RecordedAt, p.SolarRecordedAt} {
		if recorded == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, recorded); err != nil {
			return fmt.Errorf("forecast_priors recorded times must be RFC3339")
		}
	}
	if p.AnnualKWh != nil {
		parts := 0.0
		if p.IncludesEV != nil && *p.IncludesEV && p.EVAnnualKWh != nil {
			parts += *p.EVAnnualKWh
		}
		if p.IncludesHeating != nil && *p.IncludesHeating && p.HeatingAnnualKWh != nil {
			parts += *p.HeatingAnnualKWh
		}
		if parts > *p.AnnualKWh {
			return fmt.Errorf("forecast_priors: included EV and heating kWh exceed annual_kwh")
		}
	}
	return nil
}
