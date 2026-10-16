package cars

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol"
)

// An edit to a vehicles[] profile in config.yaml reaches its car, and only
// what changed: a name the owner gave the car since stays, and so does a
// choice the owner made after the edit.
func TestAProfileEditReachesItsCar(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	p := Profile{ID: "volvo", Name: "Volvo", CapacityWh: 40000, Identifiers: []string{"04:AB"}}
	h.start(p)
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Name: strp("Family car")}); err != nil {
		t.Fatal(err)
	}
	edited := p
	edited.CapacityWh, edited.Identifiers, edited.SurplusOnly, edited.TargetSoC = 42000, []string{"04:AB", "rfid-9"}, true, .7
	h.rt.Reload([]Profile{edited})
	got := h.only()
	if got.Name != "Family car" || got.CapacityWh != 42000 || !slices.Equal(got.Identifiers, []string{"04:AB", "rfid-9"}) || !got.SurplusOnly {
		t.Fatalf("after the edit %+v", got)
	}
	if _, err := h.rt.SetGoal(car.ID, nil, false, boolp(false)); err != nil {
		t.Fatal(err)
	}
	h.rt.Reload([]Profile{edited})
	if h.only().SurplusOnly {
		t.Fatal("a reload with no edit undid the owner's choice")
	}
}

// Removing a car leaves its drivers unbound: they never move to the car that
// is left.
func TestARemovedCarsDriverStaysUnbound(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	volvo := h.add(protocol.Car{Name: "Volvo", Sources: []string{"volvo-cloud"}})
	if err := h.rt.Remove(volvo.ID); err != nil {
		t.Fatal(err)
	}
	h.report("volvo-cloud", .4, h.now, "")
	h.tick(time.Second)
	if got := h.car(tesla.ID); slices.Contains(got.Sources, "volvo-cloud") || got.SocSource == protocol.CarSoCVehicle {
		t.Fatalf("the removed car's driver went to the car left: %+v", got)
	}
}

// A visitor charges on the charger's battery size, and a config reload keeps
// it so rather than leaving none.
func TestAReloadKeepsAVisitorsCapacity(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	if _, err := h.rt.UpdateCar(h.only().ID, Update{Identifiers: &[]string{"rfid-7"}}); err != nil {
		t.Fatal(err)
	}
	h.rt.ChargerIdentified("easee", "rfid-guest")
	h.plug("easee", "s1")
	cfgs := homeConfig()
	cfgs[0].VehicleCapacityWh = 60000
	h.mgr.Load(cfgs)
	if st := h.state("easee"); !st.SoCUnknown || st.VehicleCapacityWh != 60000 {
		t.Fatalf("visitor after the reload: unknown=%v capacity %v", st.SoCUnknown, st.VehicleCapacityWh)
	}
}

// The car ops check everything before they change anything; a tap may not
// take a car another charger has; an id comes back from its tombstone; and
// the box keeps at most MaxCars cars.
func TestCarOpsRefuseBeforeTheyChange(t *testing.T) {
	t.Run("a bad add does not adopt", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		minted := h.only()
		vin := "5YJ3E7EB7KF000001"
		if _, err := h.rt.UpdateCar(minted.ID, Update{VIN: &vin}); err != nil {
			t.Fatal(err)
		}
		_, err := h.rt.Add(protocol.Car{ID: newID(), Name: "Tesla", VIN: vin, CapacityWh: 5})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("err %v", err)
		}
		if got := h.only(); got.ID != minted.ID || !got.BoxMinted {
			t.Fatalf("a refused add took the box's car: %+v", got)
		}
	})
	t.Run("a car on another charger", func(t *testing.T) {
		h := newHarness(t, []loadpoint.Config{{ID: "easee", DriverName: "easee"}, {ID: "garage", DriverName: "garage"}}, nil, nil)
		h.start()
		first := h.rt.Cars()[0]
		h.plug("easee", "s1")
		if h.state("easee").CarID != first.ID {
			t.Fatalf("easee %+v", h.state("easee"))
		}
		h.mgr.ObserveSession("garage", true, 0, 0, true, "garage:G1", "g1")
		h.rt.Tick()
		if _, err := h.rt.SetSessionCar("garage", h.rt.ChargerCar("garage").Session, first.ID); !errors.Is(err, ErrElsewhere) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a removed id comes back", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		gone := h.add(protocol.Car{Name: "Old"})
		if err := h.rt.Remove(gone.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.rt.UpdateCar(h.only().ID, Update{NewID: &gone.ID}); err != nil {
			t.Fatal(err)
		}
		if got := h.rt.Removed(); len(got) != 0 {
			t.Fatalf("the live car's id is still a tombstone: %+v", got)
		}
	})
	t.Run("at most MaxCars", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		for len(h.rt.Cars()) < MaxCars {
			h.add(protocol.Car{Name: "Car"})
		}
		if _, err := h.rt.Add(protocol.Car{ID: newID(), Name: "One more"}); !errors.Is(err, ErrTooMany) {
			t.Fatalf("err %v", err)
		}
		for range maxTombstones + 5 {
			c := h.rt.Cars()[1]
			if err := h.rt.Remove(c.ID); err != nil {
				t.Fatal(err)
			}
			h.add(protocol.Car{Name: "Again"})
		}
		if got := len(h.rt.Removed()); got != maxTombstones {
			t.Fatalf("%d tombstones, want %d", got, maxTombstones)
		}
	})
}

// A level the box adopts from the loadpoint keeps its reading's time and the
// energy since: once more than 1 kWh has flowed it is an estimate, not "from
// the car" or the person's own number.
func TestAnAdoptedLevelKeepsItsReadingsTime(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.plug("easee", "s1")
	h.mgr.SetCurrentSoC("easee", .40)
	setAt := h.now
	h.deliver("easee", "s1", 3000)
	h.start()
	h.tick(time.Second)
	car := h.only()
	if car.SocSource != protocol.CarSoCManual || !car.SocEstimate || car.EnergySinceWh < 2990 || car.EnergySinceWh > 3010 {
		t.Fatalf("adopted %+v energy %d", car, car.EnergySinceWh)
	}
	if want := int64(h.now.Sub(setAt) / time.Second); car.SocAgeS == nil || *car.SocAgeS != want {
		t.Fatalf("age %v, want %d", car.SocAgeS, want)
	}
	if want := permille(0.40 + 3000*loadpoint.DefaultChargeEfficiency/76000); *car.SocPermille != want {
		t.Fatalf("shown %d, want the estimate %d", *car.SocPermille, want)
	}
}

func permille(f float64) int64 { return int64(math.Round(f * 1000)) }

// A reading more than an hour older than the plug-in does not start the
// session, whenever it arrives: the car may have driven since.
func TestAReadingFromLongBeforeThePlugInIsNotItsStart(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.report("tesla-ha", .62, h.now, "")
	h.tick(time.Second)
	h.tick(2 * time.Hour)
	h.plug("easee", "s1")
	if st := h.state("easee"); st.SoCSource != "assumed" || math.Abs(st.CurrentSoC-.2) > 1e-9 {
		t.Fatalf("a reading two hours old started the session: %s %v", st.SoCSource, st.CurrentSoC)
	}
	// The same reading arriving late changes nothing either.
	h.report("tesla-ha", .55, h.now.Add(-90*time.Minute), "")
	h.tick(time.Second)
	if st := h.state("easee"); st.SoCSource != "assumed" {
		t.Fatalf("a late old reading anchored the session: %+v", st.SoCSource)
	}
}

// A cable that flaps for one reading keeps its session, its car and its
// level; what flowed before the flap stays the car's. A new charger session
// is a new plug-in.
func TestAFlapKeepsTheSessionAndItsLevel(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.report("tesla-ha", .60, h.now, "Charging")
	h.tick(time.Second)
	h.deliver("easee", "s1", 500)
	session := h.state("easee").Session

	h.flap("easee")
	h.plug("easee", "s1")
	st := h.state("easee")
	want := .60 + 500*loadpoint.DefaultChargeEfficiency/76000
	if st.Session != session || st.CarID != car.ID || math.Abs(st.CurrentSoC-want) > 1e-6 {
		t.Fatalf("after the flap: session %q (was %q) car %q soc %v want %v", st.Session, session, st.CarID, st.CurrentSoC, want)
	}
	h.deliver("easee", "s1", 700)
	if got := h.car(car.ID); got.EnergySinceWh < 1190 || got.EnergySinceWh > 1210 || !got.SocEstimate {
		t.Fatalf("the energy before the flap is lost: %+v", got)
	}

	h.flap("easee")
	h.plug("easee", "s2")
	if st := h.state("easee"); st.Session == session {
		t.Fatal("a new charger session kept the old plug-in")
	}
}

// A box with two chargers names the car it made from each at that charger,
// as before cars, and tells listeners when it asks. A car added since could
// be on either, and the box asks.
func TestTheUsualChargerNamesItsCar(t *testing.T) {
	two := []loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000}, {ID: "garage", DriverName: "garage", VehicleCapacityWh: 40000}}
	h := newHarness(t, two, nil, nil)
	h.start()
	cars := h.rt.Cars()
	h.plug("easee", "s1")
	if st := h.state("easee"); st.CarID != cars[0].ID || st.CarIdentifiedBy != protocol.CarIdentifiedByOnly {
		t.Fatalf("easee %+v", st)
	}

	added := newHarness(t, two, nil, nil)
	added.start()
	added.add(protocol.Car{Name: "Volvo"})
	changes := 0
	added.rt.OnChange(func() { changes++ })
	added.plug("easee", "s1")
	before := changes
	added.tick(61 * time.Second)
	if st := added.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByAsking {
		t.Fatalf("with a car added since: %+v", st.CarIdentifiedBy)
	}
	if changes == before {
		t.Fatal("asking did not tell the listeners")
	}
}

// A goal set on a charger in the moment before the box opened its session
// lands on the car the box can name there, or on the session alone; never on
// the car the charger last had.
func TestARouteBeforeTheTickActsOnTheSession(t *testing.T) {
	goal := loadpoint.NewSchedule(.7, 360, false, 0, false)
	t.Run("one car", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		car := h.only()
		h.mgr.ObserveSession("easee", true, 0, 0, true, "easee:EH1", "s1")
		if _, err := h.mgr.SetScheduleChecked("easee", goal); err != nil {
			t.Fatal(err)
		}
		if got := h.car(car.ID); got.Goal == nil || got.Goal.TargetPermille != 700 {
			t.Fatalf("car %+v", got.Goal)
		}
	})
	t.Run("two cars", func(t *testing.T) {
		h := newHarness(t, homeConfig(), nil, nil)
		h.start()
		tesla := h.only()
		h.add(protocol.Car{Name: "Volvo"})
		h.mgr.ObserveSession("easee", true, 0, 0, true, "easee:EH1", "s1")
		if _, err := h.mgr.SetScheduleChecked("easee", goal); err != nil {
			t.Fatal(err)
		}
		if got := h.car(tesla.ID); got.Goal != nil {
			t.Fatalf("the car the charger last had took the goal: %+v", got.Goal)
		}
		h.tick(time.Second)
		if st := h.state("easee"); st.CarID != "" || st.Schedule.SoC != .7 {
			t.Fatalf("the session's own goal %+v car %q", st.Schedule, st.CarID)
		}
	})
}
