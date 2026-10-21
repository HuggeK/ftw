package sitesim

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func TestFuseBlowsOnTheGGCurve(t *testing.T) {
	for _, tc := range []struct{ multiple, wantS float64 }{{1, math.Inf(1)}, {2, 600}, {5, 20}, {10, 3}} {
		f := Fuse{RatingA: 25}
		blewAt := math.Inf(1)
		for s := 1; s <= 4000 && math.IsInf(blewAt, 1); s++ {
			f.step([3]float64{-tc.multiple * 25, 0, 0}, 1) // Export heats it too.
			if f.Blown[0] {
				blewAt = float64(s)
			}
		}
		if blewAt != tc.wantS || f.Blown[1] {
			t.Errorf("%.0f× rating: blew after %v s, want %v s", tc.multiple, blewAt, tc.wantS)
		}
	}
	if got := ggTripSeconds(1.4); got <= 1800 || got >= 3600 {
		t.Errorf("1.4×: %.0f s, want between the 1.25× and 1.6× points", got)
	}
}

// PV export shows as negative phase currents, or as magnitudes on an
// unsigned meter, and a slow charger and a slow battery answer late.
func TestPlantSignsAndDelays(t *testing.T) {
	p := New(t0, 25)
	p.HouseW = [3]float64{460, 460, 460}
	p.PVW = -6900
	p.Meter.Unsigned = true
	p.Battery = &Battery{Phases: 1, MaxChargeW: 3000, CapacityWh: 10000, SoC: .5, Latency: 2 * time.Second}
	p.EVSE = &EVSE{Phases: 3, MinA: 6, MaxA: 16, Delay: 5 * time.Second, ReadbackLag: 10 * time.Second}
	p.Car = &Car{Phases: 3, MaxW: 11040, Plugged: true}
	p.Events = []Event{{At: 3 * time.Second, Kind: MeterSilent, For: 2 * time.Second}}
	if tick := p.Step(); tick.Meter.W != -5520 || tick.Meter.PhaseA != [3]float64{8, 8, 8} || p.A[0] != -8 {
		t.Fatalf("export: meter %+v, physical %v", tick.Meter, p.A)
	}
	p.Battery.Command(p.Now, 2300)
	p.EVSE.Command(p.Now, 6900, "", 0, 230)
	var heard []float64
	for s := 2; s <= 17; s++ {
		tick := p.Step()
		if (tick.Meter == nil) != (s == 3 || s == 4) {
			t.Fatalf("%d s: meter reading %v", s, tick.Meter)
		}
		if want := s >= 3; (p.Battery.W == 2300) != want {
			t.Fatalf("%d s: battery %.0f W", s, p.Battery.W)
		}
		wantL2 := -8.0
		if s >= 6 {
			wantL2 = 2
		}
		if p.A[1] != wantL2 {
			t.Fatalf("%d s: L2 %.1f A, want %.0f A", s, p.A[1], wantL2)
		}
		heard = append(heard, tick.EV.W)
	}
	if heard[13] != 0 || heard[14] != 6900 {
		t.Fatalf("readback %v, want the draw from 6 s at 16 s", heard)
	}
}
