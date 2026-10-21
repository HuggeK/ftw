package sitesim

import "math"

// Fuse is a gG main fuse (IEC 60269) on each phase. Every second above its
// rating spends 1/t of its life, with t the curve's time to blow at that
// multiple of the rating; below the rating it recovers over fuseCooldownS.
// A phase blows when its damage reaches 1. Ported from the iec_gg profile in
// srcfl/device-simulator cmd/simulator/fuse.go. The curve blows slowly from
// just above the rating, so it is stricter than a real gG fuse below 1.25 ×
// its rating.
type Fuse struct {
	RatingA float64
	Damage  [3]float64
	Blown   [3]bool
}

// ggCurve is the time to blow, in seconds, at a multiple of the rating.
var ggCurve = [...]struct{ multiple, seconds float64 }{
	{1.25, 3600}, {1.6, 1800}, {2, 600}, {3, 120}, {5, 20}, {10, 3}, {20, .5},
}

const (
	fuseCooldownS = 300
	fuseMinTripS  = .05
)

// AnyBlown reports whether any phase has blown.
func (f *Fuse) AnyBlown() bool { return f.Blown[0] || f.Blown[1] || f.Blown[2] }

func (f *Fuse) step(amps [3]float64, dt float64) {
	for i, a := range amps {
		if f.Blown[i] {
			continue
		}
		ratio := math.Abs(a) / f.RatingA
		if ratio <= 1 {
			f.Damage[i] = math.Max(0, f.Damage[i]-dt/fuseCooldownS)
			continue
		}
		f.Damage[i] = math.Min(1, f.Damage[i]+dt/math.Max(fuseMinTripS, ggTripSeconds(ratio)))
		f.Blown[i] = f.Damage[i] >= 1
	}
}

// ggTripSeconds interpolates the curve on log-log axes and extends its last
// segment above 20 ×.
func ggTripSeconds(multiple float64) float64 {
	if multiple <= ggCurve[0].multiple {
		return ggCurve[0].seconds
	}
	i := 1
	for i < len(ggCurve)-1 && multiple > ggCurve[i].multiple {
		i++
	}
	a, b := ggCurve[i-1], ggCurve[i]
	t := (math.Log10(multiple) - math.Log10(a.multiple)) / (math.Log10(b.multiple) - math.Log10(a.multiple))
	return math.Pow(10, math.Log10(a.seconds)+(math.Log10(b.seconds)-math.Log10(a.seconds))*t)
}
