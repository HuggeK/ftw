package cloudlink

import (
	"bytes"
	"crypto/ecdsa"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// carSite is the fake box with cars.
type carSite struct {
	*fakeSite
	carMu sync.Mutex
	cars  []protocol.Car
	calls int
}

func (s *carSite) Cars() ([]protocol.Car, bool) {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	s.calls++
	return slices.Clone(s.cars), true
}

func (s *carSite) change(f func(c *protocol.Car)) {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	c := s.cars[0]
	f(&c)
	s.cars = []protocol.Car{c}
}

const volvoID = "0123456789abcdef0123456789abcdef"

func newCarHarness(t *testing.T, svc *fakeService) (*harness, *carSite) {
	t.Helper()
	soc, age := int64(420), int64(60)
	cs := &carSite{fakeSite: &fakeSite{grid: 1200, state: protocol.SourceLive}, cars: []protocol.Car{{
		ID: volvoID, Name: "Familjens Volvo", Make: "Volvo", VIN: "YV1XZK7V5N2000001", CapacityWh: 40000,
		SocPermille: &soc, SocSource: protocol.CarSoCVehicle, SocAgeS: &age,
		Goal: &protocol.CarGoal{TargetPermille: 800, ReadyByMin: 420, Days: 0x7f}, Identifiers: []string{"rfid-7"},
		Sources: []string{"volvo-cloud"},
	}}}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.Site = cs })
	h.site = cs.fakeSite
	return h, cs
}

func readCars(t *testing.T, env cloudwire.Envelope) cloudwire.Cars {
	t.Helper()
	var c cloudwire.Cars
	if err := cloudwire.Unmarshal(env.B, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The car class goes with the copy under ftw.live.read, by the grant's
// opaque id, and nothing in it names the car.
func TestTheCarClassGoesByAnOpaqueIdAndNamesNoCar(t *testing.T) {
	svc := newFakeService(t)
	h, _ := newCarHarness(t, svc)
	h.grant(t)
	env := svc.waitFor(cloudwire.MsgCar, 5*time.Second)
	cars := readCars(t, env)
	gk := h.record(t).GrantKey
	if len(cars.Cars) != 1 || cars.Cars[0].ID != cloudwire.CarID(gk, volvoID) || *cars.Cars[0].SocPermille != 420 ||
		cars.Cars[0].Goal == nil || cars.Cars[0].Goal.TargetPermille != 800 {
		t.Fatalf("car %+v", cars)
	}
	for _, name := range []string{"Familjens Volvo", "Volvo", "YV1XZK7V5N2000001", "rfid-7", "volvo-cloud", volvoID} {
		if bytes.Contains(env.B, []byte(name)) {
			t.Fatalf("the car class names the car: %q", name)
		}
	}
}

// Without ftw.live.read in the grant, no car class goes, and hello does not
// name it.
func TestTheCarClassNeedsLive(t *testing.T) {
	svc := newFakeService(t)
	h, cs := newCarHarness(t, svc)
	h.grant(t, protocol.ScopeEventsRead)
	svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	for _, env := range svc.received() {
		if env.T == cloudwire.MsgCar {
			t.Fatal("the car class went without ftw.live.read")
		}
	}
	cs.carMu.Lock()
	defer cs.carMu.Unlock()
	if cs.calls != 0 {
		t.Fatalf("the link read the cars %d times with no class to send", cs.calls)
	}
}

// A goal set goes at once, within lp's period; energy flowing into the car
// does not send the class at every reading.
func TestTheCarClassGoesOnAChangeAndNotOnEveryReading(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		svc.write(conn, nil, home, cloudwire.MsgWatch, cloudwire.Watch{Hz: 1})
	}
	h, cs := newCarHarness(t, svc)
	h.grant(t)
	svc.waitFor(cloudwire.MsgCar, 5*time.Second)

	cs.change(func(c *protocol.Car) { c.EnergySinceWh = 60 })
	time.Sleep(2500 * time.Millisecond)
	for _, env := range svc.received() {
		if env.T == cloudwire.MsgCar && readCars(t, env).Cars[0].EnergySinceWh == 60 {
			t.Fatal("60 Wh into the car sent the class")
		}
	}

	cs.change(func(c *protocol.Car) { c.Goal = &protocol.CarGoal{TargetPermille: 600, ReadyByMin: 390, Days: 0x1f} })
	start := time.Now()
	for {
		cars := readCars(t, svc.waitFor(cloudwire.MsgCar, 4*time.Second))
		if g := cars.Cars[0].Goal; g != nil && g.TargetPermille == 600 {
			break
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a new goal took %s", d)
	}
}

func TestTheCarChangeTestIgnoresAgesAndCountsEnergyAsLPDoes(t *testing.T) {
	soc, age, older := int64(500), int64(5), int64(900)
	a := cloudwire.Cars{Cars: []cloudwire.Car{{ID: "c", SocPermille: &soc, SocAgeS: &age, EnergySinceWh: 1000}}}
	b := cloudwire.Cars{Cars: []cloudwire.Car{{ID: "c", SocPermille: &soc, SocAgeS: &older, EnergySinceWh: 1099}}}
	if carsChanged(a, b) {
		t.Fatal("an age and 99 Wh count as a change")
	}
	b.Cars[0].EnergySinceWh = 1100
	if !carsChanged(a, b) {
		t.Fatal("100 Wh does not count")
	}
	b.Cars[0].EnergySinceWh = 1000
	moved := int64(509)
	b.Cars[0].SocPermille = &moved
	if carsChanged(a, b) {
		t.Fatal("an estimate that moved 9 ‰ counts")
	}
	moved = 510
	if !carsChanged(a, b) {
		t.Fatal("10 ‰ does not count")
	}
	b.Cars[0].SocPermille = &soc
	b.Cars[0].SocEstimate = true
	if !carsChanged(a, b) {
		t.Fatal("an exact level turning into an estimate does not count")
	}
	if !carsChanged(a, cloudwire.Cars{Cars: append(slices.Clone(a.Cars), cloudwire.Car{ID: "d"})}) {
		t.Fatal("a new car does not count")
	}
}

// A resync for full sends the car class again, unchanged, as it sends lp.
func TestAResyncSendsTheCarClassAgain(t *testing.T) {
	svc := newFakeService(t)
	svc.afterHello = func(conn uplinkConn, home string) {
		go func() {
			time.Sleep(700 * time.Millisecond)
			svc.write(conn, nil, home, cloudwire.MsgResync, cloudwire.Resync{What: []string{cloudwire.ResyncFull}})
		}()
	}
	h, _ := newCarHarness(t, svc)
	h.grant(t)
	first := svc.waitFor(cloudwire.MsgCar, 5*time.Second)
	again := svc.waitFor(cloudwire.MsgCar, 3*time.Second)
	if !bytes.Equal(readCarsKey(t, first), readCarsKey(t, again)) {
		t.Fatal("the resync sent another car class")
	}
}

func readCarsKey(t *testing.T, env cloudwire.Envelope) []byte {
	t.Helper()
	c := readCars(t, env)
	for i := range c.Cars {
		c.Cars[i].SocAgeS = nil
	}
	raw, err := cloudwire.Marshal(c.Cars)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
