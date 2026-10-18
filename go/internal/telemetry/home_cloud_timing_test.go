package telemetry

import (
	"math"
	"testing"
	"time"
)

func homeReceiptSample(s *Store, name string, kind DerType, watts float64, received time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordHomeObservation(name, kind, watts, nil, received)
	s.readings[key(name, kind)] = &DerReading{Driver: name, DerType: kind, RawW: watts, UpdatedAt: received}
	s.health[name] = &DriverHealth{Name: name}
}

func TestHomeEstimateCloudPollDoesNotAdvancePowerTime(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 120; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		power := 6000.0
		if sec >= 10 && sec <= 20 {
			power += float64(sec-10) * 76.8
		} else if sec > 20 {
			power = 6768
		}
		homeReceiptSample(s, "site", DerMeter, 1000+power, at)
		if sec == 10 {
			homeSample(s, "cloud", DerEV, 6000, at, at, true)
		}
		if sec >= 20 {
			homeSample(s, "cloud", DerEV, 6768, base.Add(20*time.Second), at, true)
		}
	}
	got := s.HomeUseEstimate(base.Add(120*time.Second), "site", ForecastOptions{})
	if got.Watts == nil || got.ToMS != base.Add(20*time.Second).UnixMilli() || got.AgeMS != 105000 || got.TimingBasis != "mixed" {
		t.Fatalf("fresh HTTP replies manufactured fresh power evidence: %+v", got)
	}
}

func TestHomeEstimateOneMinuteCloudDelayUsesPastWindow(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 150; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		ev := func(sec int) float64 {
			if sec >= 30 && sec < 70 {
				return 9714
			}
			return 0
		}
		homeReceiptSample(s, "site", DerMeter, 1000+ev(sec), at)
		if sec >= 60 {
			homeSample(s, "cloud", DerEV, ev(sec-60), at.Add(-time.Minute), at, true)
		}
		if sec < 70 {
			continue
		}
		got := s.HomeUseEstimate(at, "site", ForecastOptions{})
		if got.Watts == nil || got.ToMS != at.Add(-time.Minute).UnixMilli() || got.AgeMS != 65000 || math.Abs(*got.Watts-1000) > 1e-8 || got.SourceAgeMS != 60000 {
			t.Fatalf("second %d: cannot compare the reported past window: %+v", sec, got)
		}
	}
}

func TestHomeEstimateRejectsChargerWithoutSourceTime(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 20; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeReceiptSample(s, "site", DerMeter, 8000, at)
		homeReceiptSample(s, "cloud", DerEV, 7000, at)
	}
	if got := s.HomeUseEstimate(base.Add(20*time.Second), "site", ForecastOptions{}); got.Watts != nil {
		t.Fatalf("a charger receipt time was presented as measurement alignment: %+v", got)
	}
}
