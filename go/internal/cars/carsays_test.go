package cars

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// twoCars is Fredrik's box with a second car: the Tesla reads through Home
// Assistant (tesla-ha) and the Volvo through its cloud. With two cars rule 3
// cannot answer, so only the cars' own word or a tap names one.
func twoCars(t *testing.T) (*harness, protocol.Car, protocol.Car) {
	t.Helper()
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	tesla := h.only()
	if _, err := h.rt.UpdateCar(tesla.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, Sources: []string{"volvo-cloud"}})
	return h, tesla, volvo
}

// says is a vehicle driver's report of its car's cable and place, made now.
func (h *harness) says(driver string, plugged bool, since time.Time, atSite *bool) {
	h.obs[driver] = telemetry.VehicleObservation{Driver: driver, SoC: .4, At: h.now,
		Plugged: boolp(plugged), PluggedSince: since, AtSite: atSite, ReportedAt: h.now}
}

// A car whose source reports minutes after the plug-in still names itself
// when its own clock puts the cable within a minute of it, even as the
// source's first report since the box started.
func TestTheCarsOwnClockSaysWhenItsCableWentIn(t *testing.T) {
	h, tesla, _ := twoCars(t)
	h.plug("easee", "s1")
	plugAt := h.now
	h.tick(90 * time.Second)
	if st := h.state("easee"); st.CarID != "" || len(h.asked) != 1 {
		t.Fatalf("with no word from a car the box asks: %+v asked %d", st, len(h.asked))
	}
	h.says("tesla-ha", true, plugAt.Add(5*time.Second), boolp(true))
	h.tick(time.Second)
	if st := h.state("easee"); st.CarID != tesla.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCar {
		t.Fatalf("the car's own clock: %+v", st)
	}
}

// Two cars that both say they were plugged in within the minute are no
// answer, whether or not they know where they are: the box asks.
func TestTwoCarsClaimingThePlugInIsNoAnswer(t *testing.T) {
	for _, atSite := range []*bool{boolp(true), nil} {
		h, _, _ := twoCars(t)
		h.says("tesla-ha", false, time.Time{}, atSite)
		h.says("volvo-cloud", false, time.Time{}, atSite)
		h.tick(time.Second)
		h.plug("easee", "s1")
		h.says("tesla-ha", true, h.now.Add(10*time.Second), atSite)
		h.says("volvo-cloud", true, h.now.Add(20*time.Second), atSite)
		h.tick(20 * time.Second)
		if st := h.state("easee"); st.CarID != "" || st.CarIdentifiedBy == protocol.CarIdentifiedByCar {
			t.Fatalf("at_site %v: two claims named a car: %+v", atSite, st)
		}
		h.tick(time.Minute)
		if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByAsking || len(h.asked) != 1 {
			t.Fatalf("at_site %v: the box should ask: %+v asked %d", atSite, st, len(h.asked))
		}
	}
}

// A car that says it is somewhere else is never named, even when it says
// its cable went in at the same minute and it is the only car.
func TestACarAwayIsNeverNamed(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.says("tesla-ha", false, time.Time{}, boolp(false))
	h.tick(time.Second)
	h.plug("easee", "s1")
	h.says("tesla-ha", true, h.now.Add(5*time.Second), boolp(false))
	for i := 0; i < 4; i++ {
		h.tick(30 * time.Second)
		if st := h.state("easee"); st.CarID == car.ID {
			t.Fatalf("a car away was named: %+v", st)
		}
	}
	if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByNone {
		t.Fatalf("the only car is away, so this is a visitor: %+v", st)
	}
}

// A car that does not know where it is counts only by the minute: a cable
// that went in five minutes before this plug-in is another plug-in.
func TestACarThatDoesNotKnowWhereItIsCountsOnlyByTheMinute(t *testing.T) {
	h, tesla, _ := twoCars(t)
	h.plug("easee", "s1")
	h.says("tesla-ha", true, h.now.Add(-5*time.Minute), nil)
	h.tick(90 * time.Second)
	if st := h.state("easee"); st.CarID != "" || st.CarIdentifiedBy != protocol.CarIdentifiedByAsking {
		t.Fatalf("an old plug-in named the car: %+v", st)
	}
	h.unplug("easee")
	h.says("tesla-ha", false, h.now, nil)
	h.tick(time.Second)
	h.plug("easee", "s2")
	h.says("tesla-ha", true, h.now.Add(15*time.Second), nil)
	h.tick(20 * time.Second)
	if st := h.state("easee"); st.CarID != tesla.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByCar {
		t.Fatalf("a fresh plug-in with no place: %+v", st)
	}
}

// A source that replays a reading from before the plug-in has not seen it:
// its old "cable out" or "elsewhere" does not make the only car a visitor.
// A reading made after the plug-in does.
func TestAReadingFromBeforeThePlugInSaysNothingAboutIt(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	readAt := h.now
	h.tick(10 * time.Minute)
	h.plug("easee", "s1")
	// Home Assistant has not read the car since it came home.
	h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .4, At: readAt,
		Plugged: boolp(false), AtSite: boolp(false), Stale: true, ReportedAt: h.now.Add(10 * time.Second)}
	h.tick(10 * time.Second)
	if st := h.state("easee"); st.CarID != car.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByOnly {
		t.Fatalf("an old reading turned the only car into a visitor: %+v", st)
	}
	// Then it reads the car, 20 s after the plug-in: elsewhere.
	h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .4, At: h.now.Add(10 * time.Second),
		Plugged: boolp(false), AtSite: boolp(false), ReportedAt: h.now.Add(10 * time.Second)}
	h.tick(10 * time.Second)
	if st := h.state("easee"); st.CarID != "" || st.CarIdentifiedBy != protocol.CarIdentifiedByNone {
		t.Fatalf("a reading after the plug-in says the car is elsewhere: %+v", st)
	}
}

// Two plug-ins on two chargers within a minute: a cable time cannot say
// which charger a car is on. The review's case: the Volvo on the carport
// Easee, the Tesla on the garage charger 30 s later, and the Tesla's report
// arriving first. Matching by time alone named the Tesla on the Easee and
// the Volvo in the garage. Both chargers ask instead.
func TestTwoPlugInsWithinAMinuteAreNoCableAnswer(t *testing.T) {
	h := newHarness(t, []loadpoint.Config{{ID: "easee", DriverName: "easee", VehicleCapacityWh: 76000},
		{ID: "garage", DriverName: "garage", VehicleCapacityWh: 40000}}, nil, nil)
	h.start()
	for _, c := range h.rt.Cars()[1:] {
		if err := h.rt.Remove(c.ID); err != nil {
			t.Fatal(err)
		}
	}
	tesla := h.only()
	if _, err := h.rt.UpdateCar(tesla.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, Sources: []string{"volvo-cloud"}})
	h.says("tesla-ha", false, time.Time{}, boolp(true))
	h.says("volvo-cloud", false, time.Time{}, boolp(true))
	h.tick(time.Second)

	volvoIn := h.now
	h.plug("easee", "s1")
	h.tick(30 * time.Second)
	teslaIn := h.now
	h.mgr.ObserveSession("garage", true, 0, 0, true, "garage:G1", "g1")
	h.rt.Tick()
	h.tick(5 * time.Second)
	h.says("tesla-ha", true, teslaIn.Add(2*time.Second), boolp(true))
	h.tick(5 * time.Second)
	h.says("volvo-cloud", true, volvoIn.Add(2*time.Second), boolp(true))
	h.tick(5 * time.Second)
	for _, lp := range []string{"easee", "garage"} {
		if st := h.state(lp); st.CarID != "" || st.CarIdentifiedBy == protocol.CarIdentifiedByCar {
			t.Fatalf("%s was named by a cable time: %+v", lp, st)
		}
	}
	h.tick(time.Minute)
	if a, b := h.state("easee"), h.state("garage"); a.CarIdentifiedBy != protocol.CarIdentifiedByAsking ||
		b.CarIdentifiedBy != protocol.CarIdentifiedByAsking {
		t.Fatalf("both chargers should ask: easee %s, garage %s", a.CarIdentifiedBy, b.CarIdentifiedBy)
	}
}

// A tap over the box's own visitor verdict is final: the rule that found
// the only car elsewhere must not undo it at the next tick.
func TestATapOverTheBoxsVisitorVerdictIsFinal(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.says("tesla-ha", false, time.Time{}, boolp(false))
	h.tick(5 * time.Second)
	if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByNone {
		t.Fatalf("the only car said it is away: %+v", st)
	}
	if _, err := h.tap("easee", car.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.tick(15 * time.Second)
		// The source reads the car again, still within the minute, and
		// still says elsewhere: newer than the tap.
		h.says("tesla-ha", false, time.Time{}, boolp(false))
		h.tick(time.Second)
		if st := h.state("easee"); st.CarID != car.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByTap {
			t.Fatalf("the tap was undone: %+v", st)
		}
	}
}

// A car whose source read it elsewhere after the plug-in is no candidate:
// with two cars and one away, the other is the only car that can be here.
func TestACarAwayIsNoCandidate(t *testing.T) {
	h, tesla, _ := twoCars(t)
	h.plug("easee", "s1")
	h.says("volvo-cloud", false, time.Time{}, boolp(false))
	h.tick(5 * time.Second)
	if st := h.state("easee"); st.CarID != tesla.ID || st.CarIdentifiedBy != protocol.CarIdentifiedByOnly {
		t.Fatalf("the Volvo is away, so the Tesla is the only car: %+v", st)
	}
	// Both away: a visitor.
	again, _, _ := twoCars(t)
	again.plug("easee", "s1")
	again.says("volvo-cloud", false, time.Time{}, boolp(false))
	again.says("tesla-ha", false, time.Time{}, boolp(false))
	again.tick(5 * time.Second)
	if st := again.state("easee"); st.CarID != "" || st.CarIdentifiedBy != protocol.CarIdentifiedByNone {
		t.Fatalf("both cars away is a visitor: %+v", st)
	}
}

// A source that saw the cable change between two readings more than a
// minute apart cannot date it: no cable answer, the box asks.
func TestAChangeBetweenReadingsFarApartHasNoTime(t *testing.T) {
	for _, gap := range []time.Duration{30 * time.Second, 5 * time.Minute} {
		h, tesla, _ := twoCars(t)
		h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .4, At: h.now,
			ChargingState: "Disconnected", ReportedAt: h.now}
		h.tick(gap - 10*time.Second)
		h.plug("easee", "s1")
		h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .4, At: h.now.Add(5 * time.Second),
			ChargingState: "Stopped", ReportedAt: h.now.Add(5 * time.Second)}
		h.tick(10 * time.Second)
		named := h.state("easee").CarID == tesla.ID
		if want := gap <= time.Minute; named != want {
			t.Fatalf("gap %v: named %v, want %v (%+v)", gap, named, want, h.state("easee"))
		}
	}
}
