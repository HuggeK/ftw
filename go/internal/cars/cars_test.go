package cars

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

const teslaVIN = "XP7YGCEK7SB736799"

// Fredrik's box becomes Car 1 on first start: the loadpoint's capacity, its
// goal and its surplus choice, and the Home Assistant Tesla driver bound to
// it, VIN and all. The charger keeps showing the same goal.
func TestHomeBoxBecomesCarOneBoundToTeslaHA(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()

	car := h.only()
	if car.Name != "Car 1" || car.CapacityWh != 76000 || car.SurplusOnly || len(car.ID) != 32 {
		t.Fatalf("car 1 = %+v", car)
	}
	if car.Goal == nil || *car.Goal != (protocol.CarGoal{TargetPermille: 800, ReadyByMin: 420, Days: 0x7F}) {
		t.Fatalf("goal = %+v, want 80 %% by 07:00 every day", car.Goal)
	}

	h.ident["tesla-ha"] = [2]string{"Tesla", teslaVIN}
	h.report("tesla-ha", .14, h.now.Add(-9*time.Minute), "Stopped")
	h.tick(time.Second)
	car = h.only()
	if len(car.Sources) != 1 || car.Sources[0] != "tesla-ha" || car.VIN != teslaVIN || car.Make != "Tesla" {
		t.Fatalf("tesla-ha not bound to car 1: %+v", car)
	}
	st := h.state("easee")
	if !goalEqual(st.Schedule, homeGoal(t)) || st.VehicleCapacityWh != 76000 || st.SurplusOnly {
		t.Fatalf("the charger lost what the owner set: %+v", st.Schedule)
	}

	// The cars are stored: a restart reads the same car back.
	again := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, h.kv)
	again.start()
	if got := again.only(); got.ID != car.ID || len(got.Sources) != 1 {
		t.Fatalf("after restart: %+v", got)
	}
}

func goalEqual(a, b loadpoint.Schedule) bool { return sameGoal(goalOnly(a), goalOnly(b)) }

// The bug on Fredrik's box: the Tesla read 14 % nine minutes ago and nothing
// has flowed since, yet the level showed as inferred because a reading was
// trusted for five minutes. A reading stays the car's until energy flows.
func TestANineMinuteOldReadingWithNothingDeliveredStaysFromTheCar(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()
	h.ident["tesla-ha"] = [2]string{"Tesla", teslaVIN}
	h.plug("easee", "s1")
	h.report("tesla-ha", .14, h.now.Add(time.Minute), "Stopped")
	h.tick(time.Minute)
	h.tick(9 * time.Minute)

	car := h.only()
	if car.SocPermille == nil || *car.SocPermille != 140 || car.SocSource != protocol.CarSoCVehicle ||
		car.SocEstimate || car.EnergySinceWh != 0 {
		t.Fatalf("nine minutes, 0 Wh: %+v", car)
	}
	view, ok := h.rt.Session("easee")
	if !ok || view.Source != protocol.CarSoCVehicle || view.Estimate {
		t.Fatalf("session view %+v", view)
	}
	if st := h.state("easee"); math.Abs(st.CurrentSoC-.14) > 1e-9 || st.CarID != car.ID ||
		st.CarIdentifiedBy != protocol.CarIdentifiedByOnly {
		t.Fatalf("charger %+v", st)
	}
	if got := h.rt.PlanningLimit("easee"); got != 0 {
		t.Fatalf("limit %v from a report without one", got)
	}

	// Past 1 kWh the same reading is an estimate: "≈ … from the car + kWh".
	h.deliver("easee", "s1", 1500)
	car = h.only()
	if !car.SocEstimate || car.EnergySinceWh != 1500 || car.SocSource != protocol.CarSoCVehicle {
		t.Fatalf("after 1.5 kWh: %+v", car)
	}
	want := .14 + 1500*loadpoint.DefaultChargeEfficiency/76000
	if math.Abs(float64(*car.SocPermille)/1000-want) > .001 {
		t.Fatalf("level %d, want %.3f", *car.SocPermille, want)
	}
}

// A level read while the car stood unplugged starts the session, and the
// session's energy is credited to the car when it ends. A level from before
// the car last left a charger is not carried: the car may have driven.
func TestALevelCarriesIntoTheSessionOnlyUntilTheCarLeaves(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	h.report("tesla-ha", .50, h.now.Add(-20*time.Minute), "Disconnected")
	h.tick(time.Second)
	h.plug("easee", "s1")
	if st := h.state("easee"); math.Abs(st.CurrentSoC-.50) > 1e-9 || st.SoCSource == "assumed" {
		t.Fatalf("parked reading not carried: %+v", st)
	}
	h.deliver("easee", "s1", 7600)
	h.unplug("easee")
	car := h.only()
	if car.EnergySinceWh != 7600 || !car.SocEstimate {
		t.Fatalf("after the session: %+v", car)
	}
	h.tick(8 * time.Hour)
	h.plug("easee", "s2")
	st := h.state("easee")
	if st.SoCSource != "assumed" || math.Abs(st.CurrentSoC-.2) > 1e-9 {
		t.Fatalf("a level from before a drive was carried: %+v", st)
	}
	if car := h.only(); car.SocSource != protocol.CarSoCAssumed || !car.SocEstimate {
		t.Fatalf("car level %+v", car)
	}
}

// Two cars on one Easee, and neither the charger nor the cars say which is
// plugged in. For a minute the session is a visitor's; then the box asks,
// once. A tap names the car, and the loadpoint the planner reads takes that
// car's capacity, goal and surplus choice.
func TestTwoCarsAskWhichThenTheTapSetsThePlan(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, SurplusOnly: true,
		Goal: &protocol.CarGoal{TargetPermille: 600, ReadyByMin: 390, Days: 0x1F}})

	h.plug("easee", "s1")
	st := h.state("easee")
	if !st.SoCUnknown || st.CarID != "" || st.CarIdentifiedBy != protocol.CarIdentifiedByNone || !st.SurplusOnly ||
		!goalOnly(st.Schedule).Empty() {
		t.Fatalf("an unnamed car charges as a visitor: %+v", st)
	}
	h.tick(30 * time.Second)
	if len(h.asked) != 0 {
		t.Fatal("asked before the charger and the cars had their minute")
	}
	h.tick(31 * time.Second)
	h.tick(time.Minute)
	if len(h.asked) != 1 || h.asked[0].LoadpointID != "easee" || len(h.asked[0].CarIDs) != 2 ||
		h.asked[0].CarIDs[0] != tesla.ID || h.asked[0].CarIDs[1] != volvo.ID {
		t.Fatalf("car.which = %+v, want once with both cars", h.asked)
	}

	held, err := h.tap("easee", volvo.ID)
	if err != nil || held.CarID != volvo.ID || held.CarIdentifiedBy != protocol.CarIdentifiedByTap {
		t.Fatalf("tap: %+v %v", held, err)
	}
	h.tick(time.Second)
	st = h.state("easee")
	if st.VehicleCapacityWh != 40000 || !st.SurplusOnly || st.CarID != volvo.ID || st.SoCUnknown {
		t.Fatalf("the plan does not follow the Volvo: %+v", st)
	}
	clock, _ := st.Schedule.TimeOfDayMin()
	if st.Schedule.SoC != .6 || clock != 390 || !st.Schedule.Recurring || st.Schedule.Days != 0x1F {
		t.Fatalf("goal %+v", st.Schedule)
	}
	if st.TargetSoC != .6 || st.TargetTime.IsZero() {
		t.Fatalf("the roll did not take the Volvo's goal: target %v at %v", st.TargetSoC, st.TargetTime)
	}
	if v := h.car(volvo.ID); v.SocSource != protocol.CarSoCAssumed || !v.SocEstimate {
		t.Fatalf("the Volvo has no reading, so its level is assumed: %+v", v)
	}
	if len(h.asked) != 1 {
		t.Fatal("asked twice in one plug-in")
	}
}

// A visitor's car never gets a guessed level: not at plug-in, not after
// energy flows, not on the watch. A goal someone sets for it lasts the
// session and never becomes a car's.
func TestAVisitorNeverGetsAGuessedLevel(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()
	carOne := h.only()
	h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000})
	h.plug("easee", "s1")
	h.deliver("easee", "s1", 3000)
	st := h.state("easee")
	// The configured capacity converts the energy, so a goal set for the
	// visitor gets closer and a manual hold can release; the level itself
	// is never shown.
	if !st.SoCUnknown || st.SoCSource != "unknown" || st.VehicleCapacityWh != 76000 ||
		math.Abs(st.CurrentSoC-3000*loadpoint.DefaultChargeEfficiency/76000) > 1e-9 {
		t.Fatalf("visitor %+v", st)
	}
	if _, ok := h.rt.Session("easee"); ok {
		t.Fatal("a visitor's session has a car view")
	}

	visitorGoal := loadpoint.NewSchedule(.5, 360, false, 0, false)
	if ok, err := h.mgr.SetScheduleChecked("easee", visitorGoal); !ok || err != nil {
		t.Fatalf("goal: %v %v", ok, err)
	}
	h.tick(time.Second)
	if g := h.car(carOne.ID).Goal; g == nil || g.TargetPermille != 800 {
		t.Fatalf("the visitor's goal became car 1's: %+v", g)
	}
	if st := h.state("easee"); st.Schedule.SoC != .5 || !st.SoCUnknown {
		t.Fatalf("the visitor's own goal was not kept: %+v", st.Schedule)
	}
	h.unplug("easee")
	if st := h.state("easee"); !goalEqual(st.Schedule, homeGoal(t)) || st.SurplusOnly {
		t.Fatalf("after the visitor left the charger shows %+v", st.Schedule)
	}
}

// The tap was wrong. Moving the session to the other car moves its energy:
// the first car is back where it was, and the second has what was delivered.
func TestAMidSessionSwitchMovesTheEnergy(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000})
	if _, err := h.rt.UpdateCar(tesla.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.report("tesla-ha", .50, h.now.Add(-10*time.Minute), "Disconnected")
	h.tick(time.Second)

	h.plug("easee", "s1")
	if _, err := h.tap("easee", tesla.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	h.deliver("easee", "s1", 5000)
	if got := h.car(tesla.ID); got.EnergySinceWh != 5000 {
		t.Fatalf("tesla before the switch %+v", got)
	}

	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	if got := h.car(tesla.ID); got.EnergySinceWh != 0 || *got.SocPermille != 500 {
		t.Fatalf("tesla kept energy it never got: %+v", got)
	}
	v := h.car(volvo.ID)
	if v.EnergySinceWh != 5000 {
		t.Fatalf("volvo did not get the session's energy: %+v", v)
	}
	want := .2 + 5000*loadpoint.DefaultChargeEfficiency/40000
	if st := h.state("easee"); math.Abs(st.CurrentSoC-want) > 1e-9 || st.VehicleCapacityWh != 40000 ||
		st.CarCorrectedFrom != tesla.ID {
		t.Fatalf("charger %+v, want level %.4f", st, want)
	}
	h.unplug("easee")
	if got := h.car(volvo.ID); got.EnergySinceWh != 5000 {
		t.Fatalf("volvo after unplug %+v", got)
	}
	if got := h.car(tesla.ID); got.EnergySinceWh != 0 {
		t.Fatalf("tesla after unplug %+v", got)
	}
}

// The identification order: the charger's own word first, and a later,
// better rule corrects a tap and says so.
func TestTheChargerNamesTheCarAndALaterRuleCorrectsATap(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, Identifiers: []string{"04:AB:CD:EF"}})

	h.plug("easee", "s1")
	h.rt.ChargerIdentified("easee", "04:ab:cd:ef")
	h.tick(time.Second)
	if st := h.state("easee"); st.CarID != volvo.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCharger {
		t.Fatalf("charger rule: %+v", st)
	}
	h.unplug("easee")

	// Next plug-in: nobody says. Someone taps the Tesla, but it is the
	// Volvo, and its source reports it charging here.
	if _, err := h.rt.UpdateCar(volvo.ID, Update{Sources: &[]string{"volvo-cloud"}}); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Minute)
	h.plug("easee", "s2")
	if _, err := h.tap("easee", tesla.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	h.obs["volvo-cloud"] = telemetry.VehicleObservation{Driver: "volvo-cloud", SoC: .3, At: h.now,
		ChargingState: "Charging", AtSite: boolp(true), ReportedAt: h.now}
	h.deliver("easee", "s2", 500)
	st := h.state("easee")
	if st.CarID != volvo.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCar || st.CarCorrectedFrom != tesla.ID {
		t.Fatalf("correction: %+v", st)
	}
	if got := h.rt.ChargerCar("easee"); got.CarCorrectedFrom != tesla.ID {
		t.Fatalf("the correction is not shown: %+v", got)
	}
}

// Rule 2: a car whose own source saw the cable go in within a minute of the
// plug-in names itself. A car somewhere else does not.
func TestTheCarNamesItselfWithinAMinuteOfThePlugIn(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, Sources: []string{"volvo-cloud"}})
	if _, err := h.rt.UpdateCar(tesla.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	unplugged := func(d string, atSite *bool) telemetry.VehicleObservation {
		return telemetry.VehicleObservation{Driver: d, SoC: .4, At: h.now, Plugged: boolp(false), AtSite: atSite, ReportedAt: h.now}
	}
	h.obs["tesla-ha"] = unplugged("tesla-ha", boolp(true))
	h.obs["volvo-cloud"] = unplugged("volvo-cloud", boolp(false))
	h.tick(time.Second)

	h.plug("easee", "s1")
	// The Volvo is plugged in at work at the same moment; it says where.
	away := unplugged("volvo-cloud", boolp(false))
	away.Plugged, away.ReportedAt = boolp(true), h.now
	h.obs["volvo-cloud"] = away
	here := unplugged("tesla-ha", boolp(true))
	here.Plugged, here.ReportedAt = boolp(true), h.now.Add(20*time.Second)
	h.obs["tesla-ha"] = here
	h.tick(20 * time.Second)
	if st := h.state("easee"); st.CarID != tesla.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCar {
		t.Fatalf("car rule: %+v (volvo %s)", st, volvo.ID)
	}
	h.tick(2 * time.Minute)
	if len(h.asked) != 0 {
		t.Fatal("asked although the car named itself")
	}
}

// A restart in the middle of a session keeps the session's car and its
// capacity, so the loadpoint restores its own level.
func TestARestartKeepsTheSessionsCar(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000})
	volvo := h.rt.Cars()[1]
	h.plug("easee", "s1")
	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	h.deliver("easee", "s1", 2000)
	h.tick(2 * time.Minute) // past the level save interval

	again := newHarness(t, homeConfig(), nil, h.kv)
	again.now = h.now.Add(time.Minute)
	again.start()
	again.wh["easee"] = 2000
	again.deliver("easee", "s1", 0)
	again.tick(time.Second)
	st := again.state("easee")
	if st.CarID != volvo.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByTap || st.VehicleCapacityWh != 40000 {
		t.Fatalf("after restart: %+v", st)
	}
	if len(again.asked) != 0 {
		t.Fatal("asked again after a restart")
	}
}

// vehicles[] profiles become cars with their identifiers once. One charger
// and one profile is one car: the profile names it to the charger, and it is
// also the only car, so the charger keeps its goal and nothing asks.
func TestProfilesBecomeCars(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start(Profile{ID: "ioniq", Name: "Ioniq", CapacityWh: 58000, Identifiers: []string{"RFID-9"}, SurplusOnly: true})
	car := h.only()
	if car.Name != "Ioniq" || car.Identifiers[0] != "RFID-9" || car.CapacityWh != 58000 || car.SurplusOnly ||
		car.Goal == nil || car.Goal.TargetPermille != 800 || !car.BoxMinted {
		t.Fatalf("merged car %+v", car)
	}
	h.rt.Reload([]Profile{{ID: "ioniq", Name: "Ioniq"}, {ID: "zoe", Name: "Zoe", Identifiers: []string{"RFID-2"}}})
	if got := h.rt.Cars(); len(got) != 2 || got[1].Name != "Zoe" {
		t.Fatalf("after reload %+v", got)
	}

	// Two profiles from the start are two cars beside the charger's own.
	two := newHarness(t, homeConfig(), nil, nil)
	two.start(Profile{ID: "a", Name: "A", Identifiers: []string{"1"}}, Profile{ID: "b", Name: "B", Identifiers: []string{"2"}})
	if got := two.rt.Cars(); len(got) != 3 {
		t.Fatalf("cars %+v", got)
	}
}

// One release of overlap: a goal, a surplus choice and a level set through
// the charger's routes are the car's.
func TestTheChargerRoutesActOnTheCar(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()
	id := h.only().ID
	if ok, err := h.mgr.SetScheduleChecked("easee", loadpoint.NewSchedule(.9, 450, true, 0, false)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if prev, ok, err := h.mgr.SetSurplusOnlyChecked("easee", true); prev || !ok || err != nil {
		t.Fatal(prev, ok, err)
	}
	car := h.car(id)
	if car.Goal == nil || car.Goal.TargetPermille != 900 || car.Goal.ReadyByMin != 450 || !car.SurplusOnly {
		t.Fatalf("car %+v goal %+v", car, car.Goal)
	}
	h.plug("easee", "s1")
	if !h.mgr.SetCurrentSoC("easee", .37) {
		t.Fatal("SetCurrentSoC")
	}
	if car := h.car(id); car.SocSource != protocol.CarSoCManual || *car.SocPermille != 370 || car.SocEstimate {
		t.Fatalf("level %+v", car)
	}
	var stored record
	raw, _ := h.kv.LoadConfig(StoreKey)
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Cars[0].Goal.SoC != .9 {
		t.Fatalf("not stored: %v %s", err, raw)
	}
}

// car.goal.set, car.soc.set and car.remove on a car that is not plugged in.
func TestCarOps(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	id := h.only().ID
	car, err := h.rt.SetGoal(id, &protocol.CarGoal{TargetPermille: 700, ReadyByMin: 480}, false, boolp(true))
	if err != nil || car.Goal == nil || car.Goal.Days != 0 || !car.SurplusOnly {
		t.Fatalf("goal %+v %v", car, err)
	}
	if st := h.state("easee"); st.Schedule.SoC != .7 || st.Schedule.Recurring || !st.SurplusOnly {
		t.Fatalf("the charger the car last had did not follow: %+v", st.Schedule)
	}
	car, err = h.rt.SetLevel(id, .42)
	if err != nil || car.SocSource != protocol.CarSoCManual || *car.SocPermille != 420 {
		t.Fatalf("level %+v %v", car, err)
	}
	if _, err := h.rt.SetGoal("nope", nil, true, nil); err != ErrUnknownCar {
		t.Fatalf("unknown car: %v", err)
	}
	if _, err := h.rt.SetSessionCar("easee", "", id); err != ErrUnplugged {
		t.Fatalf("tap while unplugged: %v", err)
	}
	// The same id again is an update of that car, not a second car.
	if twin, err := h.rt.Add(protocol.Car{ID: id, Name: "Twin"}); err != nil || twin.Name != "Twin" || len(h.rt.Cars()) != 1 {
		t.Fatalf("upsert %+v %v", twin, err)
	}
	if err := h.rt.Remove(id); err != nil || len(h.rt.Cars()) != 0 {
		t.Fatalf("remove: %v", err)
	}
}

// A cars row the box cannot read is kept aside, and the cars are made again
// from the chargers' own rows, which the box keeps writing for this reason.
func TestAnUnreadableRowIsKeptAndTheCarsMadeAgain(t *testing.T) {
	kv := &memKV{rows: map[string]string{StoreKey: "{not json"}}
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, kv)
	h.start()
	if car := h.only(); car.Goal == nil || car.Goal.TargetPermille != 800 || car.CapacityWh != 76000 {
		t.Fatalf("car made again %+v", car)
	}
	if raw, _ := kv.LoadConfig(StoreKey + ":unreadable"); raw != "{not json" {
		t.Fatalf("the unreadable row was not kept: %q", raw)
	}
}
