package cloudlink

import (
	"bytes"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// A charger's car goes by an id cut with the key, as its charger and driver
// do, so neither FTW Cloud nor an agent learns the box's car ids.
func TestProjectLPCutsTheCarIds(t *testing.T) {
	gk := []byte("grant key")
	tesla, volvo := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	in := cloudwire.LP{Loadpoints: []cloudwire.Loadpoint{
		{ID: "easee", Src: "easee", CarID: tesla, CarIdentifiedBy: protocol.CarIdentifiedByTap, CarCorrected: true, CarCorrectedFrom: volvo},
		{ID: "garage", Src: "garage", CarIdentifiedBy: protocol.CarIdentifiedByAsking, Choose: []string{tesla, volvo}},
	}}
	out := ProjectLP(gk, in)
	a, b := out.Loadpoints[0], out.Loadpoints[1]
	if a.CarID != cloudwire.CarID(gk, tesla) || a.CarCorrectedFrom != cloudwire.CarID(gk, volvo) || a.CarIdentifiedBy != protocol.CarIdentifiedByTap {
		t.Fatalf("easee %+v", a)
	}
	if b.CarID != "" || len(b.Choose) != 2 || b.Choose[0] != cloudwire.CarID(gk, tesla) || b.Choose[1] != cloudwire.CarID(gk, volvo) {
		t.Fatalf("garage %+v", b)
	}
	if in.Loadpoints[1].Choose[0] != tesla {
		t.Fatal("the projection changed its input")
	}
}

// The car class carries each car's level and goal by an opaque id, and
// nothing that names the car: no name, maker, VIN, identifier or source.
func TestProjectCarsCarriesNothingThatNamesACar(t *testing.T) {
	gk := []byte("agent key")
	soc, age, limit := int64(140), int64(540), int64(800)
	in := []protocol.Car{{
		ID: "0123456789abcdef0123456789abcdef", Name: "Familjebilen", Make: "Tesla", VIN: "5YJ3E7EB7KF000001",
		CapacityWh: 76000, SocPermille: &soc, SocSource: protocol.CarSoCVehicle, SocAgeS: &age, EnergySinceWh: -120,
		ChargeLimitPermille: &limit, Goal: &protocol.CarGoal{TargetPermille: 800, ReadyByMin: 420, Days: 0x7f},
		SurplusOnly: true, Identifiers: []string{"04:AB:CD"}, Sources: []string{"tesla-ha"},
	}}
	out := ProjectCars(gk, in)
	c := out.Cars[0]
	if c.ID != cloudwire.CarID(gk, in[0].ID) || *c.SocPermille != 140 || *c.SocAgeS != 540 || c.EnergySinceWh != -120 ||
		c.Goal == nil || c.Goal.TargetPermille != 800 || c.Goal.Days != 0x7f || !c.SurplusOnly {
		t.Fatalf("car %+v", c)
	}
	raw, err := cloudwire.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Familjebilen", "Tesla", "5YJ3E7EB7KF000001", "04:AB:CD", "tesla-ha", in[0].ID} {
		if bytes.Contains(raw, []byte(name)) {
			t.Fatalf("the car class names the car: %q", name)
		}
	}
	if empty := ProjectCars(gk, nil); empty.Cars == nil {
		t.Fatal("no cars should be an empty list, not null")
	}
}
