package telemetry

import (
	"testing"
	"time"
)

// Anders' site: Pixii reports the site meter and its battery; a Growatt
// inverter drops off the bus after sunset.
func andersSite(darkness Darkness) (*Store, ForecastOptions) {
	s := NewStore()
	for _, d := range []string{"pixii", "growatt"} {
		s.EnsureDriverHealth(d)
	}
	s.Update("pixii", DerMeter, 780, nil, nil)
	s.Update("pixii", DerBattery, -200, nil, nil)
	s.RecordDriverSuccess("pixii")
	s.Update("growatt", DerPV, -1500, nil, nil)
	s.DriverHealthMut("growatt").SetOffline()
	return s, ForecastOptions{PVDark: func(time.Time) Darkness { return darkness }}
}

func TestLiveLoadAndersNightOfflinePVIsZero(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	r := s.LiveMeasurementNow("pixii", opts)
	if !r.LoadKnown || r.LoadW != 980 || r.LoadQuality != LoadMeasured || r.LoadReason != "" {
		t.Fatalf("night load = %v known=%t %s %q", r.LoadW, r.LoadKnown, r.LoadQuality, r.LoadReason)
	}
	if !r.PVLiveKnown || r.PVW != 0 {
		t.Fatalf("dark PV not shown as 0 W: known=%t pv=%v", r.PVLiveKnown, r.PVW)
	}
	if src := r.Sources["growatt:pv"]; !src.Dark || src.PowerW == nil || *src.PowerW != 0 {
		t.Fatal("dark source not marked 0 W")
	}
	// History and forecasts keep their own evidence.
	if r.Valid || r.Known[DerPV] || r.Reason != "offline:growatt:pv" {
		t.Fatalf("live display changed history qualification: valid=%t reason=%q", r.Valid, r.Reason)
	}
	if f := s.ForecastMeasurementNow("pixii", opts); f.Valid || f.Known[DerPV] {
		t.Fatal("forecast path assumed dark PV")
	}
}

func TestLiveLoadTwilightOfflinePVIsEstimate(t *testing.T) {
	s, opts := andersSite(DarknessTwilight)
	r := s.LiveMeasurementNow("pixii", opts)
	if !r.LoadKnown || r.LoadW != 980 || r.LoadQuality != LoadEstimated || r.LoadReason != "pv_twilight:growatt:pv" || !r.PVLiveKnown {
		t.Fatalf("twilight: %v %t %s %q", r.LoadW, r.LoadKnown, r.LoadQuality, r.LoadReason)
	}
}

func TestLiveLoadDaytimeOfflinePVIsEstimateWithUnknownPV(t *testing.T) {
	for _, opts := range []ForecastOptions{{}, {PVDark: func(time.Time) Darkness { return DarknessUnknown }}} {
		s, _ := andersSite(DarknessUnknown)
		r := s.LiveMeasurementNow("pixii", opts)
		if !r.LoadKnown || r.LoadW != 980 || r.LoadQuality != LoadEstimated || r.LoadReason != "offline:growatt:pv" {
			t.Fatalf("day: %v %t %s %q", r.LoadW, r.LoadKnown, r.LoadQuality, r.LoadReason)
		}
		if r.PVLiveKnown {
			t.Fatal("daytime offline PV shown as known")
		}
	}
}

func TestLiveLoadOfflineSiteMeterIsUnknown(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	s.DriverHealthMut("pixii").SetOffline()
	r := s.LiveMeasurementNow("pixii", opts)
	if r.LoadKnown || r.LoadQuality != LoadUnknown || r.LoadReason != "offline:pixii:meter" {
		t.Fatalf("offline meter: known=%t %s %q", r.LoadKnown, r.LoadQuality, r.LoadReason)
	}
	if r := s.LiveMeasurementNow("nometer", ForecastOptions{}); r.LoadKnown || r.LoadReason != "missing:nometer:meter" {
		t.Fatalf("missing meter: known=%t %q", r.LoadKnown, r.LoadReason)
	}
}

func TestLiveLoadStaleSiteMeterIsUnknown(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	s.Get("pixii", DerMeter).UpdatedAt = time.Now().Add(-10 * time.Minute)
	if r := s.LiveMeasurementNow("pixii", opts); r.LoadKnown || r.LoadReason != "stale:pixii:meter" {
		t.Fatalf("stale meter: known=%t %q", r.LoadKnown, r.LoadReason)
	}
}

// #1744: a loadpoint bound to a charger name with no driver.
func TestLiveLoadNonexistentChargerIsLeftOut(t *testing.T) {
	s := NewStore()
	s.EnsureDriverHealth("meter")
	s.Update("meter", DerMeter, 1500, nil, nil)
	opts := ForecastOptions{ExpectedFlows: []ForecastFlow{{Driver: "easee", DerType: DerEV}}}
	r := s.LiveMeasurementNow("meter", opts)
	if !r.LoadKnown || r.LoadW != 1500 || r.LoadQuality != LoadEstimated || r.LoadReason != "missing:easee:ev" {
		t.Fatalf("#1744: %v %t %s %q", r.LoadW, r.LoadKnown, r.LoadQuality, r.LoadReason)
	}
	if r.Valid || r.Known[DerEV] {
		t.Fatal("history qualification accepted the missing charger")
	}
}

func TestLiveLoadOfflineBatteryLeftOut(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	s.EnsureDriverHealth("bat2")
	s.Update("bat2", DerBattery, -3000, nil, nil)
	s.DriverHealthMut("bat2").SetOffline()
	r := s.LiveMeasurementNow("pixii", opts)
	if !r.LoadKnown || r.LoadW != 980 || r.LoadQuality != LoadEstimated || r.LoadReason != "offline:bat2:battery" {
		t.Fatalf("offline battery: %v %t %s %q", r.LoadW, r.LoadKnown, r.LoadQuality, r.LoadReason)
	}
}

func TestLiveLoadNegativeBalanceIsShown(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	s.Update("pixii", DerBattery, 2000, nil, nil)
	if r := s.LiveMeasurementNow("pixii", opts); !r.LoadKnown || r.LoadW != -1220 || r.LoadQuality != LoadEstimated || r.LoadReason != "invalid_balance" {
		t.Fatalf("negative balance: %v known=%t %q", r.LoadW, r.LoadKnown, r.LoadReason)
	}
}

func TestLiveLoadMisconfigurationStillShowsValue(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	opts.HouseholdInvalidReason = "unresolved_topology"
	opts.ExpectedFlows = []ForecastFlow{{Driver: "ghost", DerType: DerBattery}, {Driver: "ghost-ev", DerType: DerEV}}
	if r := s.LiveMeasurementNow("pixii", opts); !r.LoadKnown || r.LoadW != 980 || r.LoadQuality != LoadEstimated {
		t.Fatalf("misconfiguration hid the balance: %v known=%t %q", r.LoadW, r.LoadKnown, r.LoadReason)
	}
}

func TestLiveLoadDarkDoesNotHideMalformedPV(t *testing.T) {
	s, opts := andersSite(DarknessNight)
	s.DriverHealthMut("growatt").RecordSuccess()
	s.Get("growatt", DerPV).RawW = 300 // positive PV: a sign fault Update would refuse
	r := s.LiveMeasurementNow("pixii", opts)
	if r.PVLiveKnown || r.Sources["growatt:pv"].Dark || r.LoadQuality != LoadEstimated || r.LoadReason != "invalid_sign:growatt:pv" {
		t.Fatalf("sign fault hidden at night: pv=%t %s %q", r.PVLiveKnown, r.LoadQuality, r.LoadReason)
	}
}

func TestLiveLoadDuplicateChargerIsNotSubtractedTwice(t *testing.T) {
	s := NewStore()
	for _, d := range []string{"meter", "wallbox"} {
		s.EnsureDriverHealth(d)
	}
	s.Update("meter", DerMeter, 5000, nil, nil)
	s.Update("wallbox", DerEV, 3000, nil, nil)
	s.Update("wallbox", DerV2X, 3000, nil, nil)
	r := s.LiveMeasurementNow("meter", ForecastOptions{})
	if !r.LoadKnown || r.LoadW != 5000 || r.LoadQuality != LoadEstimated || r.LoadReason != "duplicate_charger:wallbox" {
		t.Fatalf("duplicate charger: %v known=%t %q", r.LoadW, r.LoadKnown, r.LoadReason)
	}
}
