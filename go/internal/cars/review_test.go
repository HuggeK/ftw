package cars

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// The level rule (docs/cars.md, The level) as a table: what a car shows
// after what happened to it. A number the car never said is never shown as
// the car's own, and a level from before the car last left a charger is an
// estimate.
func TestTheLevelRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		run      func(h *harness)
		source   string
		estimate bool
		permille int64
	}{
		{"read nine minutes ago, nothing since", func(h *harness) {
			h.plug("easee", "s1")
			h.report("tesla-ha", .14, h.now, "Stopped")
			h.tick(9 * time.Minute)
		}, protocol.CarSoCVehicle, false, 140},
		{"read, then 0.9 kWh: still the car's own number", func(h *harness) {
			h.plug("easee", "s1")
			h.report("tesla-ha", .14, h.now, "Charging")
			h.tick(time.Second)
			h.deliver("easee", "s1", 900)
		}, protocol.CarSoCVehicle, false, 140},
		{"read, then 1.5 kWh: an estimate", func(h *harness) {
			h.plug("easee", "s1")
			h.report("tesla-ha", .14, h.now, "Charging")
			h.tick(time.Second)
			h.deliver("easee", "s1", 1500)
		}, protocol.CarSoCVehicle, true, 158},
		{"read while parked, a session, unplugged", func(h *harness) {
			h.report("tesla-ha", .50, h.now, "Disconnected")
			h.tick(time.Second)
			h.plug("easee", "s1")
			h.deliver("easee", "s1", 7600)
			h.unplug("easee")
		}, protocol.CarSoCVehicle, true, 590},
		{"read during a session, unplugged, plugged again", func(h *harness) {
			h.plug("easee", "s1")
			h.report("tesla-ha", .60, h.now, "Charging")
			h.tick(time.Second)
			h.unplug("easee")
			h.tick(8 * time.Hour)
			h.plug("easee", "s2")
		}, protocol.CarSoCAssumed, true, 200},
		{"set by hand on the charger, nothing since", func(h *harness) {
			h.plug("easee", "s1")
			h.mgr.SetCurrentSoC("easee", .40)
			h.tick(time.Second)
		}, protocol.CarSoCManual, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, homeConfig(), nil, nil)
			h.start()
			h.ident["tesla-ha"] = [2]string{"Tesla", teslaVIN}
			tc.run(h)
			car := h.only()
			if car.SocSource != tc.source || car.SocEstimate != tc.estimate || car.SocPermille == nil || *car.SocPermille != tc.permille {
				t.Fatalf("got %s estimate=%v %v, want %s estimate=%v %d",
					car.SocSource, car.SocEstimate, deref(car.SocPermille), tc.source, tc.estimate, tc.permille)
			}
		})
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// A tap names the plug-in. One for a plug-in that has ended is refused, so a
// late tap from a notification never names the next car.
func TestATapForAnEndedPlugInIsRefused(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	volvo := h.add(protocol.Car{Name: "Volvo"})
	h.plug("easee", "s1")
	old := h.rt.ChargerCar("easee").Session
	h.unplug("easee")
	h.plug("easee", "s2")
	if _, err := h.rt.SetSessionCar("easee", old, volvo.ID); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a tap for an ended plug-in: %v", err)
	}
	if _, err := h.tap("easee", "nope"); !errors.Is(err, ErrUnknownCar) {
		t.Fatalf("unknown car: %v", err)
	}
}

// The box says asking while it waits for the tap. "None of these" makes the
// session a visitor's, and the only car left after a removal never takes it.
func TestAskingAndNoneOfThese(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	h.add(protocol.Car{Name: "Volvo"})
	h.plug("easee", "s1")
	h.tick(61 * time.Second)
	if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByAsking || len(st.Choose) != 2 || st.Session == "" {
		t.Fatalf("asking %+v", st)
	}
	if held, err := h.tap("easee", ""); err != nil || held.CarIdentifiedBy != protocol.CarIdentifiedByNone || held.CarID != "" {
		t.Fatalf("none of these: %+v %v", held, err)
	}
	h.tick(time.Minute)
	if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByNone || !st.SoCUnknown || len(h.asked) != 1 {
		t.Fatalf("after none of these %+v asked %d", st, len(h.asked))
	}
}

// car.which offers at most four cars, none of them on another charger; with
// more it offers none and the phone opens FTW.
func TestCarWhichOffersAtMostFourCarsNotOnAnotherCharger(t *testing.T) {
	h := newHarness(t, []loadpoint.Config{{ID: "easee", DriverName: "easee"}, {ID: "garage", DriverName: "garage"}}, nil, nil)
	h.start()
	ids := []string{h.rt.Cars()[0].ID, h.rt.Cars()[1].ID}
	for _, name := range []string{"C", "D", "E"} {
		ids = append(ids, h.add(protocol.Car{Name: name}).ID)
	}
	h.mgr.ObserveSession("garage", true, 0, 0, true, "garage:G1", "g1")
	h.rt.Tick()
	if _, err := h.rt.SetSessionCar("garage", h.rt.ChargerCar("garage").Session, ids[0]); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.tick(61 * time.Second)
	if len(h.asked) != 1 || len(h.asked[0].CarIDs) != 4 || h.asked[0].Session == "" {
		t.Fatalf("car.which %+v", h.asked)
	}
	for _, id := range h.asked[0].CarIDs {
		if id == ids[0] {
			t.Fatal("offered the car on the other charger")
		}
	}
	h.add(protocol.Car{Name: "F"})
	if got := h.rt.ChargerCar("easee").Choose; got != nil {
		t.Fatalf("five cars to choose from offers %v, want none", got)
	}
}

// Ids: the app mints them, the same id is an update, a car the box minted
// takes the app's id by VIN or by new_id, a VIN or identifier another car has
// is refused, and a removed car leaves a tombstone.
func TestCarIds(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	minted := h.only()
	if !minted.BoxMinted {
		t.Fatal("the migrated car is not marked box-minted")
	}
	if _, err := h.rt.Add(protocol.Car{Name: "No id"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("add without an id: %v", err)
	}
	if _, err := h.rt.UpdateCar(minted.ID, Update{VIN: strp(teslaVIN)}); err != nil {
		t.Fatal(err)
	}
	appID := newID()
	adopted, err := h.rt.Add(protocol.Car{ID: appID, Name: "Tesla", VIN: teslaVIN})
	if err != nil || adopted.ID != appID || adopted.BoxMinted || len(h.rt.Cars()) != 1 {
		t.Fatalf("adopt by VIN: %+v %v", adopted, err)
	}
	if h.rt.rec.LastCar["easee"] != appID {
		t.Fatal("the charger's last car still names the minted id")
	}
	if _, err := h.rt.UpdateCar(appID, Update{NewID: strp(newID())}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("re-keying an app car: %v", err)
	}
	other := h.add(protocol.Car{Name: "Volvo", Identifiers: []string{"04:AB"}})
	if _, err := h.rt.Add(protocol.Car{ID: newID(), Name: "Clone", VIN: teslaVIN}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate VIN: %v", err)
	}
	if _, err := h.rt.UpdateCar(appID, Update{Identifiers: &[]string{"04:ab"}}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate identifier: %v", err)
	}
	if err := h.rt.Remove(other.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.rt.Removed(); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("tombstones %+v", got)
	}
	h.add(protocol.Car{ID: other.ID, Name: "Volvo again"})
	if got := h.rt.Removed(); len(got) != 0 {
		t.Fatalf("a re-added car kept its tombstone: %+v", got)
	}

	// A car the box minted takes the app's id by new_id too.
	again := newHarness(t, homeConfig(), nil, nil)
	again.start()
	id := newID()
	if car, err := again.rt.UpdateCar(again.only().ID, Update{NewID: &id}); err != nil || car.ID != id || car.BoxMinted {
		t.Fatalf("new_id: %+v %v", car, err)
	}
}

func strp(s string) *string { return &s }

// Identification edges: a charger naming a car the box does not know makes a
// visitor once cars list identifiers, and names nobody before; the only car
// saying it is elsewhere makes a visitor. A tap over an answer the box found
// by itself is final.
func TestIdentificationEdges(t *testing.T) {
	t.Run("an idTag on a box whose cars list none", func(t *testing.T) {
		// OCPP 1.6 sends an idTag at every start. A one-car OCPP box with no
		// vehicles[] profile keeps charging its car to its goal.
		h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
		h.start()
		car := h.only()
		h.rt.ChargerIdentified("easee", "FREECHARGING")
		h.plug("easee", "s1")
		st := h.state("easee")
		if st.CarID != car.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByOnly || st.SoCUnknown {
			t.Fatalf("the idTag named nobody, so the only car is here: %+v", st)
		}
		if !st.Schedule.HasTarget() || st.Schedule.SoC != 0.8 {
			t.Fatalf("the car's goal %+v", st.Schedule)
		}
	})
	t.Run("an unknown card when cars list theirs", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		car := h.only()
		if _, err := h.rt.UpdateCar(car.ID, Update{Identifiers: &[]string{"rfid-7"}}); err != nil {
			t.Fatal(err)
		}
		// The charger's word arrives before the loadpoint sees the plug-in.
		h.rt.ChargerIdentified("easee", "rfid-guest")
		h.plug("easee", "s1")
		if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByNone || st.CarID != "" || !st.SoCUnknown {
			t.Fatalf("a guest's card %+v", st)
		}
	})
	t.Run("the only car is elsewhere", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		car := h.only()
		if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
			t.Fatal(err)
		}
		h.plug("easee", "s1")
		if st := h.state("easee"); st.CarID != car.ID {
			t.Fatalf("only %+v", st)
		}
		h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .5, At: h.now,
			AtSite: boolp(false), ReportedAt: h.now.Add(10 * time.Second)}
		h.tick(10 * time.Second)
		if st := h.state("easee"); st.CarID != "" || st.CarIdentifiedBy != protocol.CarIdentifiedByNone ||
			!st.CarCorrected || st.CarCorrectedFrom != car.ID {
			t.Fatalf("the only car said it is away: %+v", st)
		}
	})
	t.Run("a tap over the box's own answer is final", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		tesla := h.only()
		volvo := h.add(protocol.Car{Name: "Volvo", Identifiers: []string{"04:AB"}, Sources: []string{"volvo-cloud"}})
		h.plug("easee", "s1")
		h.rt.ChargerIdentified("easee", "04:AB")
		h.tick(time.Second)
		if st := h.state("easee"); st.CarID != volvo.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCharger {
			t.Fatalf("charger %+v", st)
		}
		if _, err := h.tap("easee", tesla.ID); err != nil {
			t.Fatal(err)
		}
		h.obs["volvo-cloud"] = telemetry.VehicleObservation{Driver: "volvo-cloud", SoC: .3, At: h.now,
			ChargingState: "Charging", AtSite: boolp(true), ReportedAt: h.now}
		h.deliver("easee", "s1", 500)
		if st := h.state("easee"); st.CarID != tesla.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByTap {
			t.Fatalf("the tap was overruled: %+v", st)
		}
	})
}

// One car is on one charger at a time: with one car and two chargers
// plugged in, the second is a visitor's.
func TestOneCarIsOnOneCharger(t *testing.T) {
	h := newHarness(t, []loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000}, {ID: "garage", DriverName: "garage"}}, nil, nil)
	h.start()
	if err := h.rt.Remove(h.rt.Cars()[1].ID); err != nil {
		t.Fatal(err)
	}
	car := h.only()
	h.plug("easee", "s1")
	h.mgr.ObserveSession("garage", true, 0, 0, true, "garage:G1", "g1")
	h.rt.Tick()
	if a, b := h.state("easee"), h.state("garage"); a.CarID != car.ID || b.CarID != "" || !b.SoCUnknown {
		t.Fatalf("easee %q, garage %q unknown=%v", a.CarID, b.CarID, b.SoCUnknown)
	}
}

// A switch to another car gives the car it had its level from before the
// session back, even when the session had started it from an assumption.
func TestASwitchRestoresTheOldCarsLevel(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000})
	if _, err := h.rt.SetLevel(tesla.ID, .66); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.tick(61 * time.Second)
	if _, err := h.tap("easee", tesla.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	h.deliver("easee", "s1", 2000)
	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	if got := h.car(tesla.ID); got.SocSource != protocol.CarSoCManual || *got.SocPermille != 660 || got.EnergySinceWh != 0 {
		t.Fatalf("tesla after the switch %+v", got)
	}
}

// After a restart the loadpoint knows nothing of its cable until the charger
// reports. A tick in that gap must not end the session or lose its car.
func TestARestartBeforeTheChargerReportsKeepsTheSession(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000})
	h.plug("easee", "s1")
	h.tick(61 * time.Second)
	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	h.deliver("easee", "s1", 2000)
	h.tick(2 * time.Minute)

	again := newHarness(t, homeConfig(), nil, h.kv)
	again.now = h.now.Add(time.Minute)
	again.start()
	again.rt.Tick() // before the charger has reported
	again.wh["easee"] = 2000
	again.deliver("easee", "s1", 0)
	st := again.state("easee")
	if st.CarID != volvo.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByTap || st.VehicleCapacityWh != 40000 {
		t.Fatalf("after restart: %+v", st)
	}
	for _, c := range again.rt.rec.Cars {
		if c.ID == volvo.ID && c.UnpluggedMs != 0 {
			t.Fatal("the restart counted as an unplug")
		}
	}
}

// The first start with cars while a car charges keeps the level the
// loadpoint already trusts, rather than an assumption.
func TestFirstStartWhileChargingKeepsTheTrustedLevel(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.plug("easee", "s1")
	h.mgr.SetCurrentSoC("easee", .55)
	h.start()
	h.tick(time.Second)
	if st := h.state("easee"); math.Abs(st.CurrentSoC-.55) > 1e-9 || st.SoCSource == "assumed" {
		t.Fatalf("the trusted level was replaced: %+v", st.CurrentSoC)
	}
	if car := h.only(); car.SocSource != protocol.CarSoCManual || *car.SocPermille != 550 {
		t.Fatalf("car %+v", car)
	}
}

// A setting changed in config.yaml, by hand or on the box's settings page,
// is the car's for one release, as the charger routes are.
func TestASettingsEditReachesTheCar(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	capacity, surplus := 50000.0, true
	h.rt.Reload(nil, SettingsChange{LP: "easee", CapacityWh: &capacity, SurplusOnly: &surplus})
	h.tick(time.Second)
	if car := h.only(); car.CapacityWh != 50000 || !car.SurplusOnly {
		t.Fatalf("car %+v", car)
	}
	if st := h.state("easee"); st.VehicleCapacityWh != 50000 || !st.SurplusOnly {
		t.Fatalf("charger %+v", st)
	}
}

// A schedule that holds only the site's battery rule is no goal, and the
// loadpoint keeps it without the runtime applying it again each tick.
func TestTheSiteRuleAloneIsNoGoal(t *testing.T) {
	var rule loadpoint.Schedule
	if err := json.Unmarshal([]byte(`{"surplus_unlock_bat_soc":0.9,"time_of_day_min":420,"recurring":true}`), &rule); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": rule}, nil)
	h.start()
	if car := h.only(); car.Goal != nil {
		t.Fatalf("goal %+v", car.Goal)
	}
	h.tick(time.Second)
	if st := h.state("easee"); st.Schedule.SurplusUnlockBatSoC != .9 {
		t.Fatalf("the site rule was lost: %+v", st.Schedule)
	}
}
