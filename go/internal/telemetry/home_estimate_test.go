package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func homeSample(s *Store, name string, kind DerType, w float64, measured, received time.Time, confirmed bool) {
	data, _ := json.Marshal(map[string]any{"power_observed_at": measured.Format(time.RFC3339Nano), "control_power_confirmed": confirmed})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordHomeObservation(name, kind, w, data, received)
	s.readings[key(name, kind)] = &DerReading{Driver: name, DerType: kind, RawW: w, UpdatedAt: received, Data: data}
	s.health[name] = &DriverHealth{Name: name}
}

// This is a synthetic, timestamped replay of the reported failure class, not
// Göran's missing raw 19:32 trace. A four-second delivery delay is a test input.
func TestHomeEstimateAlignedReplayVersusIndependentKalman(t *testing.T) {
	s := NewStore()
	base := time.Date(2026, 10, 17, 17, 32, 0, 0, time.UTC)
	gridFilter, evFilter := NewKalman(100, 50), NewKalman(100, 50)
	var alignedError, filteredError float64
	var count int
	for second := 0; second <= 100; second++ {
		at := base.Add(time.Duration(second) * time.Second)
		ev := func(sec int) float64 {
			if sec >= 10 && sec < 40 || sec >= 60 && sec < 75 {
				return 9714
			}
			return 0
		}
		homeSample(s, "site", DerMeter, 1000+ev(second), at, at, false)
		grid := gridFilter.Update(1000 + ev(second))
		if second >= 4 {
			measured := at.Add(-4 * time.Second)
			homeSample(s, "ev", DerEV, ev(second-4), measured, at, false)
			evFilter.Update(ev(second - 4))
		}
		if second < 14 {
			continue
		}
		out := s.HomeUseEstimate(at, "site", ForecastOptions{})
		if out.Watts == nil {
			t.Fatalf("second %d: %+v", second, out)
		}
		alignedError += math.Abs(*out.Watts - 1000)
		filteredError += math.Abs(grid - evFilter.Estimate - 1000)
		if out.ToMS != at.Add(-4*time.Second).UnixMilli() || out.AgeMS != 9000 {
			t.Fatal(out)
		}
		count++
	}
	if alignedError > 1e-8 || filteredError/float64(count) < 500 {
		t.Fatal(alignedError, filteredError, count)
	}
	t.Logf("synthetic replay MAE: aligned %.3f W, independent scalar Kalman %.1f W", alignedError/float64(count), filteredError/float64(count))
}

func TestHomeWindowTimeWeightingAndSignedEnergy(t *testing.T) {
	base := time.Unix(1000, 0)
	for _, offsets := range [][]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, {0, 2, 9, 10}} {
		var p []ControlObservation
		for _, sec := range offsets {
			p = append(p, ControlObservation{PowerW: float64(sec) * 100, At: base.Add(time.Duration(sec) * time.Second)})
		}
		mean, low, high, ok := homeWindowPower(p, base, base.Add(10*time.Second))
		if !ok || math.Abs(mean-500) > 1e-9 || low != 0 || high != 1000 {
			t.Fatal(mean, low, high, ok)
		}
	}
	// Integrating then subtracting must equal subtracting the signed energies.
	p := []ControlObservation{{PowerW: -1000, At: base}, {PowerW: 3000, At: base.Add(10 * time.Second)}}
	mean, _, _, ok := homeWindowPower(p, base, base.Add(10*time.Second))
	if !ok || mean != 1000 {
		t.Fatal(mean, ok)
	}
}

func TestHomeEstimateChangeOnlyConfirmationAndCachedRepeats(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	original := base.Add(-time.Hour)
	for sec := 0; sec <= 20; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeSample(s, "site", DerMeter, 1500, at, at, false)
		homeSample(s, "ev", DerEV, 500, original, at, true)
	}
	got := s.HomeUseEstimate(base.Add(20*time.Second), "site", ForecastOptions{})
	if got.Watts != nil || len(s.homeObservations["ev:ev"]) != 1 {
		t.Fatal(got)
	}
	// Asking repeatedly must not advance the interval or modify its source.
	for i := 0; i < 100; i++ {
		again := s.HomeUseEstimate(base.Add(25*time.Second), "site", ForecastOptions{})
		if again.Watts != nil || again.ToMS != got.ToMS {
			t.Fatal(again)
		}
	}
	if got := s.HomeUseEstimate(base.Add(51*time.Second), "site", ForecastOptions{}); got.Watts != nil {
		t.Fatal(got)
	}
	// An unconfirmed duplicate cannot extend a plateau.
	u := NewStore()
	for sec := 0; sec <= 20; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		homeSample(u, "site", DerMeter, 1000, base, at, false)
	}
	if got := u.HomeUseEstimate(base.Add(20*time.Second), "site", ForecastOptions{}); got.Watts != nil {
		t.Fatal(got)
	}
}

func TestHomeEstimateRejectsGapsAndConflicts(t *testing.T) {
	base := time.Unix(100000, 0)
	for _, scenario := range []string{"offline", "long_gap", "backward", "conflict", "future", "unknown", "restart", "duplicate_topology"} {
		t.Run(scenario, func(t *testing.T) {
			s := NewStore()
			for sec := 0; sec <= 10; sec++ {
				at := base.Add(time.Duration(sec) * time.Second)
				homeSample(s, "site", DerMeter, 1000, at, at, false)
			}
			now := base.Add(10 * time.Second)
			opts := ForecastOptions{}
			switch scenario {
			case "offline":
				s.health["site"].SetOffline()
			case "long_gap":
				now = base.Add(50 * time.Second)
				homeSample(s, "site", DerMeter, 1000, now, now, false)
			case "backward":
				homeSample(s, "site", DerMeter, 1000, base.Add(5*time.Second), now, false)
			case "conflict":
				homeSample(s, "site", DerMeter, 2000, now, now, false)
			case "future":
				homeSample(s, "site", DerMeter, 1000, now.Add(time.Second), now, false)
			case "unknown":
				s.recordHomeObservation("site", DerMeter, 1000, json.RawMessage(`{"control_power_available":false}`), now)
			case "restart":
				s = NewStore()
				homeSample(s, "site", DerMeter, 1000, now, now, false)
			case "duplicate_topology":
				opts.ExpectedFlows = []ForecastFlow{{Driver: "site", DerType: DerMeter, FlowID: "x"}, {Driver: "site", DerType: DerMeter, FlowID: "x"}}
			}
			if got := s.HomeUseEstimate(now, "site", opts); got.Watts != nil {
				t.Fatal(got)
			}
		})
	}
}

func TestHomeEstimateDoesNotRepairRawNegativeBalance(t *testing.T) {
	s := NewStore()
	base := time.Unix(100000, 0)
	for sec := 0; sec <= 20; sec++ {
		at := base.Add(time.Duration(sec) * time.Second)
		ev := 9714.0
		grid := 10600.0
		bat := 0.0
		if sec >= 20 {
			grid = -267
			bat = -6035
		}
		homeSample(s, "site", DerMeter, grid, at, at, false)
		homeSample(s, "bat", DerBattery, bat, at, at, false)
		// The slow source has not reported the stop yet.
		if sec <= 16 {
			homeSample(s, "ev", DerEV, ev, at, at, false)
		}
	}
	now := base.Add(20 * time.Second)
	for i := 0; i < 5; i++ {
		raw := s.ForecastMeasurement(now, "site", ForecastOptions{})
		if raw.Valid || raw.Reason != "invalid_balance" || raw.HouseholdW != -3946 {
			t.Fatal(raw)
		}
		got := s.HomeUseEstimate(now, "site", ForecastOptions{})
		if got.Watts == nil || math.Abs(*got.Watts-886) > 1e-8 || got.AgeMS != 9000 {
			t.Fatal(fmt.Sprintf("%+v", got))
		}
	}
}

func TestHomeWindowCannotExtendThroughControlConfirmations(t *testing.T) {
	base := time.Unix(100000, 0)
	points := []ControlObservation{
		{At: base, ConfirmedAt: base.Add(20 * time.Second), PowerW: 1000},
		{At: base.Add(19 * time.Second), ConfirmedAt: base.Add(40 * time.Second), PowerW: 2000},
	}
	// Control confirmations are HTTP/status evidence, not source brackets.
	if _, _, _, ok := homeWindowPower(points, base.Add(15*time.Second), base.Add(25*time.Second)); ok {
		t.Fatal("overlap treated as certain")
	}
	mean, _, _, ok := homeWindowPower(points, base.Add(30*time.Second), base.Add(40*time.Second))
	if ok {
		t.Fatal("confirmation extended source evidence", mean, ok)
	}
}
