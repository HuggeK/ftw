package main

import (
	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// replanOnPlugIn asks for a new plan the moment a charger confirms a cable,
// past the reactive check and its gap, so a car plugged in gets its plan in
// the time a solve takes. The planner says when the plan turns pending and
// when the new one lands, and the plan feed takes both to the phones.
//
// The connection event already waits two seconds after an unplug, so a cable
// that flaps does not storm the optimizer, and a newer request replaces one
// still waiting. A hold enters no plan, and goals and schedules request their
// own replans where they are saved.
func replanOnPlugIn(bus *events.Bus, planner *mpc.Service) {
	if bus == nil || planner == nil {
		return
	}
	bus.Subscribe(events.KindChargingConnected, func(events.Event) {
		planner.RequestReplan("loadpoint_plugged")
	})
}
