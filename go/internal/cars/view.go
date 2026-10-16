package cars

import (
	"math"
	"slices"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol"
)

// Cars is every car, in the box's order, as the routes and the session
// carry it.
func (r *Runtime) Cars() []protocol.Car {
	byID := statesByID(r.deps.Loadpoints.States())
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.Car, 0, len(r.rec.Cars))
	for _, c := range r.rec.Cars {
		out = append(out, r.viewLocked(c, byID))
	}
	return out
}

// Car is one car.
func (r *Runtime) Car(id string) (protocol.Car, error) {
	byID := statesByID(r.deps.Loadpoints.States())
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.carLocked(id)
	if c == nil {
		return protocol.Car{}, ErrUnknownCar
	}
	return r.viewLocked(c, byID), nil
}

func (r *Runtime) viewLocked(car *Car, byID map[string]loadpoint.State) protocol.Car {
	c := protocol.Car{
		ID: car.ID, BoxMinted: car.BoxMinted, Name: car.Name, Make: car.Make, VIN: car.VIN,
		CapacityWh: int64(car.CapacityWh + 0.5), MaxW: int64(car.MaxW + 0.5), Phases: int64(car.Phases),
		Goal: carGoal(car.Goal), SurplusOnly: car.SurplusOnly,
		Identifiers: car.Identifiers, Sources: car.Sources,
	}
	lvl := car.Level
	if lvl.Source == "" {
		return c
	}
	soc, energy, est := r.shownLevelLocked(car, byID)
	permille := units.PermilleFromFraction(soc)
	c.SocPermille = &permille
	c.SocSource = lvl.Source
	age := max(0, r.deps.Now().UnixMilli()-lvl.AtMs) / 1000
	c.SocAgeS = &age
	c.EnergySinceWh = int64(math.Round(energy))
	c.SocEstimate = est
	if lvl.ChargeLimit > 0 {
		limit := units.PermilleFromFraction(lvl.ChargeLimit)
		c.ChargeLimitPermille = &limit
	}
	return c
}

// levelNowLocked is the car's level now and the energy delivered to it since
// its reading. On a charger it is the loadpoint's estimate, which the
// planner reads too.
func (r *Runtime) levelNowLocked(car *Car, byID map[string]loadpoint.State) (soc, energySinceWh float64) {
	lvl := car.Level
	if lp, s := r.sessionOfLocked(car.ID); s != nil && s.seeded {
		if st, ok := byID[lp]; ok && st.PluggedIn && !st.SoCUnknown {
			from := 0.0
			if lvl.LP == lp {
				from = lvl.SessionWh
			}
			// Negative when a V2X session took energy out.
			return st.CurrentSoC, lvl.BaseWh + st.DeliveredWhSession - from
		}
	}
	soc = lvl.SoC
	if car.CapacityWh > 0 {
		soc += lvl.BaseWh * loadpoint.DefaultChargeEfficiency / car.CapacityWh
	}
	return units.ClampFraction(soc), lvl.BaseWh
}

// shownLevelLocked is the level a car shows and the planner starts from:
// the car's or a person's own number while it is exact, the estimate after.
// A number the car never said is never shown as the car's.
func (r *Runtime) shownLevelLocked(car *Car, byID map[string]loadpoint.State) (soc, energySinceWh float64, estimated bool) {
	now, energy := r.levelNowLocked(car, byID)
	estimated = estimate(car, energy)
	if !estimated {
		return car.Level.SoC, energy, false
	}
	return now, energy, true
}

// estimate reports whether the box counts the level as an estimate: once
// more than CarEstimateAfterWh has flowed since the reading, either way, for an assumed
// level, and for a level read before the car last left a charger, since it
// may have driven.
func estimate(car *Car, energySinceWh float64) bool {
	lvl := car.Level
	return lvl.Source == protocol.CarSoCAssumed || math.Abs(energySinceWh) > protocol.CarEstimateAfterWh ||
		(car.UnpluggedMs > 0 && lvl.AtMs < car.UnpluggedMs)
}

// ChargerCar is what a charger carries about its car.
func (r *Runtime) ChargerCar(lpID string) protocol.ChargerCar {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		return protocol.ChargerCar{}
	}
	cc := protocol.ChargerCar{Session: s.ID, CarID: s.CarID, CarIdentifiedBy: s.By,
		CarCorrected: s.CorrectedFrom != "", CarCorrectedFrom: s.CorrectedFrom}
	if s.By == protocol.CarIdentifiedByAsking {
		cc.Choose = r.chooseLocked(lpID)
	}
	return cc
}

// SessionView is the session's car as a charger route shows it.
type SessionView struct {
	CarID    string
	Source   string
	Estimate bool
	At       time.Time
	// SoC is the reading itself, and Shown the level the car shows and the
	// planner starts from: the reading while exact, the estimate after.
	SoC         float64
	Shown       float64
	ChargeLimit float64
	// Driver and ChargingState are the newest report among the car's
	// sources, and DriverAt when the car measured it.
	Driver        string
	ChargingState string
	DriverAt      time.Time
}

// Session is the car on a charger. False when nothing is plugged in or the
// session is a visitor's.
func (r *Runtime) Session(lpID string) (SessionView, bool) {
	byID := statesByID(r.deps.Loadpoints.States())
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		return SessionView{}, false
	}
	car := r.carLocked(s.CarID)
	if car == nil || car.Level.Source == "" {
		return SessionView{CarID: s.CarID}, car != nil
	}
	shown, _, est := r.shownLevelLocked(car, byID)
	v := SessionView{
		CarID: car.ID, Source: car.Level.Source, Estimate: est,
		At: car.Level.at(), SoC: car.Level.SoC, Shown: shown, ChargeLimit: car.Level.ChargeLimit,
	}
	if o, ok := r.newestLocked(car); ok {
		v.Driver, v.ChargingState, v.DriverAt = o.Driver, o.ChargingState, o.At
	}
	return v, true
}

// newestLocked is the newest report among a car's sources.
func (r *Runtime) newestLocked(car *Car) (telemetry.VehicleObservation, bool) {
	var best telemetry.VehicleObservation
	found := false
	for _, d := range car.Sources {
		ss := r.sources[d]
		if ss == nil || !ss.seen {
			continue
		}
		if !found || ss.obs.At.After(best.At) {
			best, found = ss.obs, true
		}
	}
	return best, found
}

// LiveReport is the newest live report of the car on a charger: its driver
// and charging state, for waking the car and for knowing when it is done.
// False when the session has no car or its sources have nothing live.
func (r *Runtime) LiveReport(lpID string) (telemetry.VehicleObservation, bool) {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		return telemetry.VehicleObservation{}, false
	}
	car := r.carLocked(s.CarID)
	if car == nil {
		return telemetry.VehicleObservation{}, false
	}
	o, ok := r.newestLocked(car)
	if !ok || !o.Live(now) {
		return telemetry.VehicleObservation{}, false
	}
	return o, true
}

// Sources are the vehicle drivers of the car on a charger.
func (r *Runtime) Sources(lpID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		return nil
	}
	if car := r.carLocked(s.CarID); car != nil {
		return append([]string(nil), car.Sources...)
	}
	return nil
}

// Removed are the cars an owner removed, newest last.
func (r *Runtime) Removed() []Tombstone {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rec.Removed)
}

// PlanningLimit is the charge limit the car reported, when its level is the
// car's own and still exact. A plan never caps at a limit that may be old.
func (r *Runtime) PlanningLimit(lpID string) float64 {
	v, ok := r.Session(lpID)
	if !ok || v.Source != protocol.CarSoCVehicle || v.Estimate {
		return 0
	}
	return v.ChargeLimit
}
