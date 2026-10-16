package cars

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// memKV is state.db's config table in memory.
type memKV struct {
	mu   sync.Mutex
	rows map[string]string
}

func (k *memKV) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.rows[key]
	return v, ok
}

func (k *memKV) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows[key] = value
	return nil
}

// harness is one box: a loadpoint manager, the cars runtime, the vehicle
// drivers' reports and a clock the test moves.
type harness struct {
	t     *testing.T
	now   time.Time
	kv    *memKV
	mgr   *loadpoint.Manager
	rt    *Runtime
	obs   map[string]telemetry.VehicleObservation
	ident map[string][2]string
	asked []events.CarWhich
	wh    map[string]float64
}

// homeConfig is Fredrik's box on 15 October 2026: one Easee at the carport,
// a 76 kWh car, a goal of 80 % by 07:00 every day, and no surplus-only.
func homeConfig() []loadpoint.Config {
	return []loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000, MaxChargeW: 11000, MinChargeW: 4140}}
}

func homeGoal(t *testing.T) loadpoint.Schedule {
	t.Helper()
	var s loadpoint.Schedule
	if err := json.Unmarshal([]byte(`{"soc":0.8,"time_of_day_min_utc":300,"recurring":true,"time_of_day_min":420}`), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newHarness(t *testing.T, cfgs []loadpoint.Config, goals map[string]loadpoint.Schedule, kv *memKV) *harness {
	t.Helper()
	if kv == nil {
		kv = &memKV{rows: map[string]string{}}
	}
	h := &harness{
		t: t, now: time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC), kv: kv,
		mgr: loadpoint.NewManager(), obs: map[string]telemetry.VehicleObservation{},
		ident: map[string][2]string{}, wh: map[string]float64{},
	}
	h.mgr.SetNowFn(func() time.Time { return h.now })
	h.mgr.SetLocation(time.UTC)
	h.mgr.Load(cfgs)
	if err := h.mgr.HydrateSchedules(func(id string) (loadpoint.Schedule, bool) {
		s, ok := goals[id]
		return s, ok
	}); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	bus.Subscribe(events.KindCarWhich, func(e events.Event) { h.asked = append(h.asked, e.(events.CarWhich)) })
	h.rt = New(Deps{
		Store: kv, Loadpoints: h.mgr, Bus: bus, Now: func() time.Time { return h.now },
		Vehicles: func() []telemetry.VehicleObservation {
			out := make([]telemetry.VehicleObservation, 0, len(h.obs))
			for _, o := range h.obs {
				out = append(out, o)
			}
			return out
		},
		Identity: func(d string) (string, string) { return h.ident[d][0], h.ident[d][1] },
	})
	// Wired as main.go wires them: a goal, a surplus choice or a level set
	// on a charger is its car's.
	h.mgr.SetScheduleSaver(func(id string, s loadpoint.Schedule) error {
		_, err := h.rt.GoalSaved(id, s)
		return err
	})
	h.mgr.SetSurplusOnlySaver(func(id string, v bool) error {
		_, err := h.rt.SurplusSaved(id, v)
		return err
	})
	h.mgr.SetLevelObserver(h.rt.LevelSet)
	return h
}

func (h *harness) start(profiles ...Profile) {
	h.t.Helper()
	if err := h.rt.Start(profiles); err != nil {
		h.t.Fatal(err)
	}
	// Past the window in which a plug-in counts as found at start.
	h.now = h.now.Add(time.Minute)
}

// tap is the one tap, naming the plug-in the charger shows.
func (h *harness) tap(lp, car string) (protocol.ChargerCar, error) {
	return h.rt.SetSessionCar(lp, h.rt.ChargerCar(lp).Session, car)
}

// add adds a car with an id the test mints, as the owner's app does.
func (h *harness) add(c protocol.Car) protocol.Car {
	h.t.Helper()
	if c.ID == "" {
		c.ID = newID()
	}
	got, err := h.rt.Add(c)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// tick moves the clock and runs what main.go runs each control tick: the
// chargers' observations, then the cars.
func (h *harness) tick(d time.Duration) {
	h.now = h.now.Add(d)
	h.mgr.RollSchedules(h.now)
	h.rt.Tick()
}

// plug and unplug drive the charger as the Easee driver reports it.
func (h *harness) plug(lp, session string) {
	h.wh[lp] = 0
	h.mgr.ObserveSession(lp, true, 0, 0, true, "easee:EH1", session)
	h.rt.Tick()
}

// deliver charges at 7 kW for as long as wh takes: the loadpoint does not
// believe a counter that outruns the power.
func (h *harness) deliver(lp, session string, wh float64) {
	h.wh[lp] += wh
	h.now = h.now.Add(time.Duration(wh / 7000 * float64(time.Hour)))
	h.mgr.ObserveSession(lp, true, 7000, h.wh[lp], true, "easee:EH1", session)
	h.rt.Tick()
}

// unplug is a cable out for good: the charger says so past UnplugGrace.
func (h *harness) unplug(lp string) {
	h.mgr.ObserveSession(lp, false, 0, 0, false, "easee:EH1", "")
	h.rt.Tick()
	h.now = h.now.Add(UnplugGrace)
	h.mgr.ObserveSession(lp, false, 0, 0, false, "easee:EH1", "")
	h.rt.Tick()
}

// flap is a cable that reads as out for one tick and is back.
func (h *harness) flap(lp string) {
	h.mgr.ObserveSession(lp, false, 0, 0, false, "easee:EH1", "")
	h.tick(5 * time.Second)
}

// report is a vehicle driver's level report.
func (h *harness) report(driver string, soc float64, at time.Time, state string) {
	h.obs[driver] = telemetry.VehicleObservation{Driver: driver, SoC: soc, At: at, ChargingState: state, ReportedAt: at}
}

func (h *harness) car(id string) protocol.Car {
	h.t.Helper()
	c, err := h.rt.Car(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) only() protocol.Car {
	h.t.Helper()
	cs := h.rt.Cars()
	if len(cs) != 1 {
		h.t.Fatalf("%d cars, want 1", len(cs))
	}
	return cs[0]
}

func (h *harness) state(lp string) loadpoint.State {
	h.t.Helper()
	st, ok := h.mgr.State(lp)
	if !ok {
		h.t.Fatalf("no loadpoint %s", lp)
	}
	return st
}

func boolp(v bool) *bool { return &v }
