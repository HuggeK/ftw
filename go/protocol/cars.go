package protocol

// Cars. A car belongs to its owner, not to the home, and visits chargers. The
// box keeps the cars of its owners; a charging session is one car on one
// charger from plug-in to unplug. docs/cars.md is the normative text, and
// the field names are the registry's car_fields and charger_car_fields,
// read back by TestCarStructsAreTheRegistrys.
//
// The cbor names are the session's. The json names are the box's HTTP
// routes, which spell every registry name in snake_case.

// Car is one car as the box keeps it.
type Car struct {
	// ID is 128 bits as 32 lowercase hex digits. The owner's app mints it,
	// and it is the same on every box the owner has.
	ID string `cbor:"id" json:"id"`
	// BoxMinted marks a car the box made from its old settings, with an id
	// of its own. The owner's app adopts it with car.update new_id, or by
	// adding a car with the same VIN.
	BoxMinted bool `cbor:"boxMinted,omitempty" json:"box_minted,omitempty"`
	// OwnerID is reserved for the owner the car belongs to. Until owners
	// are principals, every phone with the owner role shares one list.
	OwnerID string `cbor:"ownerId,omitempty" json:"owner_id,omitempty"`
	Name    string `cbor:"name" json:"name"`
	Make    string `cbor:"make,omitempty" json:"make,omitempty"`
	// VIN binds a vehicle driver that reports the same VIN to this car.
	VIN        string `cbor:"vin,omitempty" json:"vin,omitempty"`
	CapacityWh int64  `cbor:"capacityWh" json:"capacity_wh"`
	// MaxW and Phases are the car's own charger. Zero means unknown, and a
	// session then takes the charger's limits alone.
	MaxW   int64 `cbor:"maxW,omitempty" json:"max_w,omitempty"`
	Phases int64 `cbor:"phases,omitempty" json:"phases,omitempty"`

	// SocPermille is the level. While SocEstimate is false it is the
	// number the car or a person gave; after, it is that number plus the
	// energy delivered to the car since, × efficiency / capacity. Absent
	// when the box has none.
	SocPermille *int64 `cbor:"socPermille,omitempty" json:"soc_permille,omitempty"`
	// SocSource is where that reading came from, one of CarSoC*.
	SocSource string `cbor:"socSource,omitempty" json:"soc_source,omitempty"`
	// SocAgeS is the seconds since the car reported the level or someone set
	// it, at send. Never the box's wall clock, which may be wrong.
	SocAgeS *int64 `cbor:"socAgeS,omitempty" json:"soc_age_s,omitempty"`
	// EnergySinceWh is the energy delivered to this car since then;
	// negative when a V2X session took energy out.
	EnergySinceWh int64 `cbor:"energySinceWh" json:"energy_since_wh"`
	// SocEstimate is true once more than the box's limit of energy has
	// flowed since the reading, for a level read before the car last left
	// a charger, and always for an assumed level. A reading with nothing
	// delivered since stays exact for as long as that holds.
	SocEstimate         bool   `cbor:"socEstimate" json:"soc_estimate"`
	ChargeLimitPermille *int64 `cbor:"chargeLimitPermille,omitempty" json:"charge_limit_permille,omitempty"`

	// Goal is one per car, used wherever the car is plugged in.
	Goal *CarGoal `cbor:"goal,omitempty" json:"goal,omitempty"`
	// SurplusOnly is the car's preference: it charges from surplus PV only.
	SurplusOnly bool `cbor:"surplusOnly" json:"surplus_only"`

	// Identifiers are the RFID tags, MAC addresses and eMAIDs a charger may
	// name the car by.
	Identifiers []string `cbor:"identifiers,omitempty" json:"identifiers,omitempty"`
	// Sources are the vehicle drivers on this box that report the car's
	// level. They are the box's own and never copied to another box: Home
	// Assistant at home, the maker's service anywhere.
	Sources []string `cbor:"sources,omitempty" json:"sources,omitempty"`
}

// CarGoal is what a car should reach and when.
type CarGoal struct {
	TargetPermille int64 `cbor:"targetPermille" json:"target_permille"`
	// ReadyByMin is minutes after midnight in the box's time zone.
	ReadyByMin int64 `cbor:"readyByMin" json:"ready_by_min"`
	// Days is an ISO weekday mask, Monday in bit 0. Zero is once, at the
	// next ready time, and 127 every day. A loadpoint schedule's days reads
	// 0 as every day when recurring; docs/cars.md maps the two.
	Days uint8 `cbor:"days" json:"days"`
	// FinishAtLimit charges on until the car stops at its own limit.
	FinishAtLimit bool `cbor:"finishAtLimit" json:"finish_at_limit"`
}

// ChargerCar is what a charger carries about the car on it, beside its own
// fields: on GET /api/loadpoints and, with opaque ids, in FTW Cloud's lp
// class. The watch's NowLoadpoint carries all but CarCorrectedFrom. Empty
// while nothing is plugged in.
type ChargerCar struct {
	// Session is this plug-in's id. session.car.set names it, and the box
	// refuses a tap for a plug-in that has ended.
	Session string `cbor:"session,omitempty" json:"session,omitempty"`
	// CarID is the session's car, empty when the box does not know which.
	CarID string `cbor:"carId,omitempty" json:"car_id,omitempty"`
	// CarIdentifiedBy is one of CarIdentifiedBy*.
	CarIdentifiedBy string `cbor:"carIdentifiedBy,omitempty" json:"car_identified_by,omitempty"`
	// CarCorrected is set when a better rule replaced the car the box named
	// first, and CarCorrectedFrom is that car, for the rest of the session,
	// so the correction is shown rather than silent.
	CarCorrected     bool   `cbor:"carCorrected,omitempty" json:"car_corrected,omitempty"`
	CarCorrectedFrom string `cbor:"carCorrectedFrom,omitempty" json:"car_corrected_from,omitempty"`
	// Choose is, while the box asks, the cars a tap may pick: at most four,
	// none of them on another charger.
	Choose []string `cbor:"choose,omitempty" json:"choose,omitempty"`
}

// Operations on cars. Their scopes are in contract/registry.yaml `ops`.
// Arguments follow the box's convention: snake_case, `id` for what the op
// acts on, a level as a fraction from 0 to 1.
const (
	// OpCarGoalSet sets how and when a car charges. Args: id (the car) or
	// charger_id (a visitor's session); target, a fraction from 0 to 1, and
	// ready_by_min, with days and finish_at_limit, which together replace
	// the goal; clear, true to remove it; and surplus_only. The goal and
	// surplus_only are each optional: one that is absent stays as it was.
	OpCarGoalSet = "car.goal.set"
	// OpCarSoCSet sets a car's level by hand. Args: id or charger_id, and
	// soc, a fraction from 0 to 1.
	OpCarSoCSet = "car.soc.set"
	// OpSessionCarSet says which car is on a charger: the one tap. Args: id
	// (the charger), session (the plug-in's id) and car_id. An empty car_id
	// makes the session a visitor's.
	OpSessionCarSet = "session.car.set"
	// OpCarAdd adds a car. Args: id, which the owner's app mints, and the
	// car's own fields without the level. An id the box has is an update.
	OpCarAdd = "car.add"
	// OpCarUpdate changes a car. Args: id and the fields that change;
	// new_id re-keys a car the box minted to the id the owner's app holds.
	OpCarUpdate = "car.update"
	// OpCarRemove removes a car. Args: id. A session on it becomes a
	// visitor's, and the box keeps the id as removed.
	OpCarRemove = "car.remove"
)

// CarEstimateAfterWh is how much energy may flow after a reading before the
// box counts the level as an estimate. It replaces a time limit: a reading
// with nothing delivered since is as true after nine minutes as after one.
const CarEstimateAfterWh = 1000
