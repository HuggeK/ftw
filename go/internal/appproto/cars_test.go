package appproto

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/protocol"
)

// fakeCars answers the car ops as the cars runtime does.
type fakeCars struct {
	cars    map[string]protocol.Car
	plugged bool
	// elsewhere is a car another charger has.
	elsewhere   string
	tapped      string
	calls       int
	sessionGoal *protocol.CarGoal
}

func (f *fakeCars) Cars() []protocol.Car {
	out := make([]protocol.Car, 0, len(f.cars))
	for _, c := range f.cars {
		out = append(out, c)
	}
	return out
}

func newFakeCars() *fakeCars {
	return &fakeCars{cars: map[string]protocol.Car{"c1": {ID: "c1", Name: "Tesla"}, "c2": {ID: "c2", Name: "Volvo"}}, plugged: true}
}

func (f *fakeCars) Add(c protocol.Car) (protocol.Car, error) {
	f.calls++
	if c.ID == "" {
		c.ID = "minted"
	}
	f.cars[c.ID] = c
	return c, nil
}

func (f *fakeCars) UpdateCar(id string, u cars.Update) (protocol.Car, error) {
	f.calls++
	c, ok := f.cars[id]
	if !ok {
		return protocol.Car{}, cars.ErrUnknownCar
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	f.cars[id] = c
	return c, nil
}

func (f *fakeCars) Remove(id string) error {
	f.calls++
	if _, ok := f.cars[id]; !ok {
		return cars.ErrUnknownCar
	}
	delete(f.cars, id)
	return nil
}

func (f *fakeCars) SetGoal(id string, goal *protocol.CarGoal, clear bool, surplus *bool) (protocol.Car, error) {
	f.calls++
	c, ok := f.cars[id]
	if !ok {
		return protocol.Car{}, cars.ErrUnknownCar
	}
	if goal != nil || clear {
		c.Goal = goal
	}
	if surplus != nil {
		c.SurplusOnly = *surplus
	}
	f.cars[id] = c
	return c, nil
}

func (f *fakeCars) SetLevel(id string, soc float64) (protocol.Car, error) {
	f.calls++
	c, ok := f.cars[id]
	if !ok {
		return protocol.Car{}, cars.ErrUnknownCar
	}
	permille := int64(soc*1000 + 0.5)
	c.SocPermille, c.SocSource = &permille, protocol.CarSoCManual
	f.cars[id] = c
	return c, nil
}

func (f *fakeCars) SetSessionGoal(charger string, goal *protocol.CarGoal, clear bool, surplus *bool) (protocol.ChargerCar, error) {
	f.calls++
	f.sessionGoal = goal
	return protocol.ChargerCar{Session: "s1", CarIdentifiedBy: protocol.CarIdentifiedByNone}, nil
}

func (f *fakeCars) SetSessionLevel(charger string, soc float64) (protocol.ChargerCar, error) {
	f.calls++
	return protocol.ChargerCar{Session: "s1", CarIdentifiedBy: protocol.CarIdentifiedByNone}, nil
}

func (f *fakeCars) SetSessionCar(charger, session, car string) (protocol.ChargerCar, error) {
	f.calls++
	switch {
	case charger != "lp1":
		return protocol.ChargerCar{}, cars.ErrUnknownCharger
	case !f.plugged:
		return protocol.ChargerCar{}, cars.ErrUnplugged
	case session != "s1":
		return protocol.ChargerCar{}, cars.ErrSessionEnded
	case car != "" && f.cars[car].ID == "":
		return protocol.ChargerCar{}, cars.ErrUnknownCar
	case car != "" && car == f.elsewhere:
		return protocol.ChargerCar{}, cars.ErrElsewhere
	}
	f.tapped = car
	return protocol.ChargerCar{Session: session, CarID: car, CarIdentifiedBy: protocol.CarIdentifiedByTap}, nil
}

func newCarRig(t *testing.T) (*Handler, *fakeCars, *recorder) {
	t.Helper()
	h, _, rec, _ := newRig(t)
	c := newFakeCars()
	h.cfg.Cars = c
	return h, c, rec
}

// The one tap: a watch may make it, as it may charge. The result is the
// charger's car as the box holds it, not an echo.
func TestAWatchPicksTheCarWithOneTap(t *testing.T) {
	h, c, rec := newCarRig(t)
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = newWatchGrants()
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpSessionCarSet, "cmd-tap", map[string]any{"id": "lp1", "session": "s1", "car_id": "c2"}, 7, 200_000))

	rec.only(t, MsgCmdAck)
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || res.Charger == nil || res.Charger.CarID != "c2" ||
		res.Charger.CarIdentifiedBy != protocol.CarIdentifiedByTap || c.tapped != "c2" {
		t.Fatalf("tap result %+v, tapped %q", res, c.tapped)
	}
}

// The list of cars is the owner's. A watch and a viewer are refused before
// anything is acked; the goal and the level are a phone's to set, so a
// viewer is refused those too.
func TestOnlyAnOwnerKeepsTheListOfCars(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller func(*Handler)
		op     string
		args   map[string]any
		scope  string
	}{
		{"watch adds", func(h *Handler) { h.cfg.Caller, h.cfg.Grants = watchCaller(), newWatchGrants() },
			OpCarAdd, map[string]any{"id": "0123456789abcdef0123456789abcdef", "name": "Zoe"}, ScopeCarsWrite},
		{"watch removes", func(h *Handler) { h.cfg.Caller, h.cfg.Grants = watchCaller(), newWatchGrants() },
			OpCarRemove, map[string]any{"id": "c1"}, ScopeCarsWrite},
		{"viewer renames", func(h *Handler) { h.cfg.Caller, h.cfg.Grants = viewerCaller(), newViewerGrants() },
			OpCarUpdate, map[string]any{"id": "c1", "name": "X"}, ScopeCarsWrite},
		{"viewer sets a goal", func(h *Handler) { h.cfg.Caller, h.cfg.Grants = viewerCaller(), newViewerGrants() },
			OpCarGoalSet, map[string]any{"id": "c1", "surplus_only": true}, ScopeDispatchWrite},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, fake, rec := newCarRig(t)
			c.caller(h)
			subscribe(t, h, rec)
			deliver(t, h, MsgCmd, nil, cmdLoadpoint(c.op, "cmd-"+c.name, c.args, 7, 200_000))
			res := body[CmdResult](t, rec.only(t, MsgCmdResult))
			if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrScopeDenied ||
				res.Error.Args["needScope"] != c.scope {
				t.Fatalf("result %+v", res)
			}
			if fake.calls != 0 || rec.has(MsgCmdAck) {
				t.Fatal("a refused car op reached the cars")
			}
		})
	}
}

func TestAnOwnerAddsSetsAGoalAndALevel(t *testing.T) {
	h, c, rec := newCarRig(t)
	subscribe(t, h, rec)
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarAdd, "cmd-add", map[string]any{
		"id": "0123456789abcdef0123456789abcdef", "name": "Zoe", "capacity_wh": int64(52000),
		"identifiers": []any{"rfid-7"}, "goal": map[any]any{"target_permille": int64(800), "ready_by_min": int64(420), "days": int64(31)},
	}, 7, 200_000))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	// The result carries the car's id, level and goal, to fit lane 0; the
	// app reads its name, VIN and identifiers from GET /api/cars.
	if res.State != CmdApplied || res.Car == nil || res.Car.ID != "0123456789abcdef0123456789abcdef" ||
		res.Car.Goal == nil || res.Car.Goal.Days != 31 || res.Car.Name != "" || res.Car.Identifiers != nil {
		t.Fatalf("add %+v", res)
	}
	if added := c.cars["0123456789abcdef0123456789abcdef"]; added.Name != "Zoe" || added.CapacityWh != 52000 || len(added.Identifiers) != 1 {
		t.Fatalf("the box got %+v", added)
	}

	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarGoalSet, "cmd-goal", map[string]any{"id": "c1", "clear": true, "surplus_only": true}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || res.Car == nil || res.Car.Goal != nil || !res.Car.SurplusOnly {
		t.Fatalf("goal %+v", res)
	}

	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarSoCSet, "cmd-soc", map[string]any{"id": "c1", "soc": 0.14}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || res.Observed == nil || res.Observed.Value != 0.14 || res.Car.SocSource != protocol.CarSoCManual {
		t.Fatalf("level %+v", res)
	}

	// A visitor's session: its own goal, through charger_id.
	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarGoalSet, "cmd-session-goal", map[string]any{"charger_id": "lp1",
		"target": 0.5, "ready_by_min": int64(360), "days": int64(31)}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || res.Charger == nil || c.sessionGoal == nil || c.sessionGoal.TargetPermille != 500 ||
		c.sessionGoal.ReadyByMin != 360 || c.sessionGoal.Days != 31 {
		t.Fatalf("session goal %+v", res)
	}
	if c.calls != 4 {
		t.Fatalf("%d calls", c.calls)
	}
}

// A refusal after the ack names why in machine form: no such car, nothing
// plugged in. A bad argument is refused before the ack.
func TestCarOpRefusals(t *testing.T) {
	h, c, rec := newCarRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarGoalSet, "cmd-x", map[string]any{"id": "nope", "surplus_only": true}, 7, 200_000))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error.Code != ErrNotFound || res.Error.Args["reason"] != "car" {
		t.Fatalf("unknown car %+v", res)
	}

	// A tap for a plug-in that has ended.
	rec.reset()
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpSessionCarSet, "cmd-old", map[string]any{"id": "lp1", "session": "s0", "car_id": "c1"}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error.Code != ErrPrecondition || res.Error.Args["reason"] != "session" {
		t.Fatalf("ended plug-in %+v", res)
	}

	// A car another charger has.
	rec.reset()
	c.elsewhere = "c2"
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpSessionCarSet, "cmd-busy", map[string]any{"id": "lp1", "session": "s1", "car_id": "c2"}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error.Code != ErrPrecondition || res.Error.Args["reason"] != "elsewhere" {
		t.Fatalf("a car on another charger %+v", res)
	}

	rec.reset()
	c.plugged = false
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpSessionCarSet, "cmd-y", map[string]any{"id": "lp1", "session": "s1", "car_id": "c1"}, 7, 200_000))
	res = body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error.Code != ErrUnavailable || res.Error.Args["reason"] != "unplugged" {
		t.Fatalf("unplugged %+v", res)
	}

	for _, bad := range []struct {
		args map[string]any
		arg  string
	}{
		{map[string]any{"id": "c1", "soc": 1.01}, "soc"},
		{map[string]any{"id": "c1", "soc": "14"}, "soc"},
		{map[string]any{"soc": 0.5}, "id"},
		{map[string]any{"id": "c1", "charger_id": "lp1", "soc": 0.5}, "id"},
	} {
		rec.reset()
		deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarSoCSet, "cmd-bad", bad.args, 7, 200_000))
		res = body[CmdResult](t, rec.only(t, MsgCmdResult))
		if res.State != CmdRejected || res.Error.Code != ErrUnknownOp || res.Error.Args["arg"] != bad.arg || rec.has(MsgCmdAck) {
			t.Fatalf("bad args %v: %+v", bad.args, res)
		}
	}
	for _, bad := range []struct {
		args map[string]any
		arg  string
	}{
		{map[string]any{"id": "c1", "target": 0.5}, "ready_by_min"},
		{map[string]any{"id": "c1", "target": 1.2, "ready_by_min": int64(360)}, "target"},
		{map[string]any{"id": "c1", "target": 0.5, "ready_by_min": int64(1440)}, "ready_by_min"},
		{map[string]any{"id": "c1", "clear": true, "target": 0.5, "ready_by_min": int64(360)}, "clear"},
		{map[string]any{"id": "c1", "target": 0.5, "ready_by_min": int64(360), "days": int64(128)}, "days"},
	} {
		rec.reset()
		deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarGoalSet, "cmd-bad-goal", bad.args, 7, 200_000))
		res = body[CmdResult](t, rec.only(t, MsgCmdResult))
		if res.State != CmdRejected || res.Error.Code != ErrUnknownOp || res.Error.Args["arg"] != bad.arg || rec.has(MsgCmdAck) {
			t.Fatalf("bad goal %v: %+v", bad.args, res)
		}
	}
	for _, bad := range []struct {
		args map[string]any
		arg  string
	}{
		{map[string]any{"id": "lp1", "car_id": "c1"}, "session"},
		{map[string]any{"id": "lp1", "session": "s1"}, "car_id"},
	} {
		rec.reset()
		deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpSessionCarSet, "cmd-bad-tap", bad.args, 7, 200_000))
		res = body[CmdResult](t, rec.only(t, MsgCmdResult))
		if res.State != CmdRejected || res.Error.Args["arg"] != bad.arg || rec.has(MsgCmdAck) {
			t.Fatalf("bad tap %v: %+v", bad.args, res)
		}
	}
}

// A box without cars answers the car ops as unavailable, not unknown: the
// op exists in this build, and the subsystem is what is missing.
func TestABoxWithoutCarsSaysUnavailable(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarSoCSet, "cmd-z", map[string]any{"id": "c1", "soc": 0.1}, 7, 200_000))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error.Code != ErrUnavailable || res.Error.Args["subsystem"] != "cars" {
		t.Fatalf("result %+v", res)
	}
}

// The car a cmd.result carries fits a 512 B lane 0 bucket however full the
// car. In a 256 B session a full one does not, and the result goes without
// it rather than not at all.
func TestABriefCarFitsLaneZero(t *testing.T) {
	soc, age, limit := int64(1000), int64(9_999_999), int64(1000)
	full := protocol.Car{
		ID: "0123456789abcdef0123456789abcdef", Name: "A name of sixty-four characters, which is as long as one gets!", Make: "Volkswagen",
		VIN: "WVWZZZE1ZMP000001", CapacityWh: 300000, MaxW: 400000, Phases: 3,
		SocPermille: &soc, SocSource: protocol.CarSoCVehicle, SocAgeS: &age, EnergySinceWh: -299_999, SocEstimate: true,
		ChargeLimitPermille: &limit, Goal: &protocol.CarGoal{TargetPermille: 1000, ReadyByMin: 1439, Days: 0x7f, FinishAtLimit: true},
		SurplusOnly: true, Identifiers: []string{"04:AB:CD:EF:01:23:45", "rfid-7", "EMAID-SE-ABC-123456789-0"}, Sources: []string{"tesla-ha", "vag"},
	}
	brief := briefCar(full)
	size, err := encodedSize(MsgCmdResult, CmdResult{CmdID: "01J9ZQ7C8K3M5N7P9R1T3V5X7Z", State: CmdApplied, Car: &brief,
		Observed: &Observed{Value: 1, Src: ObservedSrcCore, UptimeMs: 1 << 40}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a full brief car in cmd.result: %d bytes", size)
	const headerBytes = 6
	if size+headerBytes > 512 {
		t.Fatalf("%d bytes do not fit the 512 B bucket", size)
	}

	h, c, rec := newCarRig(t)
	h.mu.Lock()
	h.bucket = 256
	h.mu.Unlock()
	c.cars["c1"] = full
	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpCarSoCSet, "cmd-soc", map[string]any{"id": "c1", "soc": 1.0}, 7, 200_000))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied || res.Car != nil {
		t.Fatalf("in a 256 B session %+v", res)
	}
}
