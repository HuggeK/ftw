// Package cars keeps the cars of a box's owners apart from its chargers
// (docs/cars.md). A car belongs to its owner and visits chargers; a charging
// session is one car on one loadpoint, from plug-in to unplug.
//
// The loadpoint manager keeps each session's energy and turns the car's level
// into the estimate the planner and the controller read. This package decides
// which car a session belongs to, keeps each car's level, goal and choices,
// and hands them to the loadpoint. Nothing here commands hardware.
package cars

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// StoreKey is the state.db config row that holds every car.
const StoreKey = "cars"

// IdentifyWindow is how close to a plug-in the car's own report must come for
// it to name the car, and how long the box waits for the charger or the car
// before it asks which car it is.
const IdentifyWindow = time.Minute

// levelSaveEvery bounds how often a new reading alone writes state.db. A car
// that reports every few seconds must not wear the SD card; a restart then
// loses at most this much of a level the car will report again.
const levelSaveEvery = time.Minute

// KV is the state.db config table.
type KV interface {
	LoadConfig(key string) (string, bool)
	SaveConfig(key, value string) error
}

// Deps is what the runtime needs from the box.
type Deps struct {
	Store      KV
	Loadpoints *loadpoint.Manager
	// Vehicles lists each vehicle driver's newest level report.
	Vehicles func() []telemetry.VehicleObservation
	// Identity is a driver's make and serial, as it reported them. A vehicle
	// driver reports the VIN as its serial.
	Identity func(driver string) (make, serial string)
	Bus      *events.Bus
	// Replan asks the planner to plan again. Nil in tests.
	Replan func(reason string)
	Now    func() time.Time
}

// Car is one car as the box keeps it.
type Car struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Make       string  `json:"make,omitempty"`
	VIN        string  `json:"vin,omitempty"`
	CapacityWh float64 `json:"capacity_wh,omitempty"`
	MaxW       float64 `json:"max_w,omitempty"`
	Phases     int     `json:"phases,omitempty"`
	// PluginSoC is the level the box assumes at plug-in when it has none
	// to carry. Zero is units.DefaultPluginSoC.
	PluginSoC float64 `json:"plugin_soc,omitempty"`
	// Goal is a loadpoint schedule without the site's battery rule. Empty
	// is no goal.
	Goal        loadpoint.Schedule `json:"goal"`
	SurplusOnly bool               `json:"surplus_only,omitempty"`
	Identifiers []string           `json:"identifiers,omitempty"`
	Sources     []string           `json:"sources,omitempty"`
	// ProfileID is the vehicles[] profile the car came from, and
	// ProfileTarget that profile's target without a ready time, applied to
	// each session as the profile was.
	ProfileID string `json:"profile_id,omitempty"`
	// BoxMinted marks a car the box made, with an id of its own, from its
	// old settings. The owner's app may give it the app's id.
	BoxMinted     bool    `json:"box_minted,omitempty"`
	ProfileTarget float64 `json:"profile_target,omitempty"`
	Level         Level   `json:"level"`
	// UnpluggedMs is when the car last left a charger. A level from before
	// it may predate a drive, so a new session does not carry it.
	UnpluggedMs int64 `json:"unplugged_ms,omitempty"`
	// UsualLP is the loadpoint the box made the car from. A plug-in there
	// names the car when no other car calls it usual (docs/cars.md, rule 3).
	UsualLP string `json:"usual_lp,omitempty"`
}

// Level is the car's last reading and the energy delivered since.
type Level struct {
	SoC    float64 `json:"soc"`
	Source string  `json:"source,omitempty"`
	AtMs   int64   `json:"at_ms,omitempty"`
	// BaseWh is the energy delivered to the car since AtMs in sessions
	// that have ended.
	BaseWh float64 `json:"base_wh,omitempty"`
	// LP and SessionWh place a reading inside a session: the loadpoint and
	// its session counter at AtMs. Energy delivered past SessionWh in that
	// session is the car's.
	LP          string  `json:"lp,omitempty"`
	SessionWh   float64 `json:"session_wh,omitempty"`
	ChargeLimit float64 `json:"charge_limit,omitempty"`
}

func (l Level) at() time.Time {
	if l.AtMs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(l.AtMs)
}

// session is one car, or none, on one plugged-in loadpoint.
type session struct {
	// ID names this plug-in. A tap names it, and is refused once it ended.
	ID            string `json:"id"`
	CarID         string `json:"car,omitempty"`
	By            string `json:"by"`
	CorrectedFrom string `json:"corrected_from,omitempty"`
	// Key is the charger's own session identity, when it has one.
	Key       string `json:"key,omitempty"`
	PluggedMs int64  `json:"plugged_ms"`
	BoundMs   int64  `json:"bound_ms"`
	// LastWh is the session's energy at the last tick, kept so an unplug
	// can credit it after the charger has reset its counter.
	LastWh float64 `json:"last_wh"`
	// Asked is set once car.which went out for this plug-in, or once the
	// box no longer needs to ask.
	Asked bool `json:"asked,omitempty"`
	// Tapped is set when a person named the car, or none, and TapFinal
	// when that tap overrode an answer the box had found by itself: no rule
	// corrects it then.
	Tapped   bool `json:"tapped,omitempty"`
	TapFinal bool `json:"tap_final,omitempty"`
	// Prev is the bound car's level from before this session, so a switch
	// to another car puts it back.
	Prev *Level `json:"prev,omitempty"`

	restored bool
	// uncertain marks a session the box found plugged in as it started: a
	// reading from before it may be from before the plug-in or after.
	uncertain bool
	// connMs is the loadpoint's own connection time as last seen. After a
	// restart it differs from PluggedMs, the plug-in itself.
	connMs int64
	// unpluggedAt is when the charger first said the cable is out. The
	// session ends once it has said so for UnplugGrace.
	unpluggedAt    time.Time
	chargerIdent   string
	chargerIdentAt time.Time
	seeded         bool
	// A visitor's own goal and surplus choice, until unplug.
	goal    *loadpoint.Schedule
	surplus *bool
}

// record is the StoreKey row.
type record struct {
	Version  int                 `json:"version"`
	Cars     []*Car              `json:"cars"`
	Sessions map[string]*session `json:"sessions,omitempty"`
	// LastCar is the car each loadpoint last had: what its routes act on
	// while nothing is plugged in.
	LastCar map[string]string `json:"last_car,omitempty"`
	// Profiles maps each vehicles[] profile id to the car made from it, and
	// ProfileSeen holds each profile as the box last applied it, so an edit
	// in config.yaml reaches the car and nothing else the owner set does.
	Profiles    map[string]string  `json:"profiles,omitempty"`
	ProfileSeen map[string]Profile `json:"profile_seen,omitempty"`
	// Unbound are vehicle drivers the owner took from a car. The box does
	// not bind them again by itself.
	Unbound []string `json:"unbound,omitempty"`
	// Migrated is set once the box has made cars from its loadpoints. A box
	// with no loadpoint yet makes them when the first one is configured.
	Migrated bool `json:"migrated,omitempty"`
	// Removed are the cars an owner removed, so an app copying lists
	// between boxes does not bring them back.
	Removed []Tombstone `json:"removed,omitempty"`
}

// Tombstone is a removed car.
type Tombstone struct {
	ID   string `json:"id"`
	AtMs int64  `json:"at_ms"`
}

// sourceState follows one vehicle driver's word on its cable.
type sourceState struct {
	seen         bool
	plugged      bool
	pluggedSince time.Time
	obs          telemetry.VehicleObservation
}

// Runtime keeps the cars and their sessions. Safe for concurrent use.
type Runtime struct {
	// syncMu orders the runtime's decisions with the loadpoint calls that
	// carry them out: a tap and a tick never hand the loadpoint their
	// bindings or seeds out of order. Taken before mu, and never by the
	// loadpoint's savers and observers.
	syncMu    sync.Mutex
	mu        sync.Mutex
	deps      Deps
	rec       record
	sources   map[string]*sourceState
	applied   map[string]loadpoint.CarBinding
	listeners []func()
	dirty     bool
	levelSave bool
	lastSave  time.Time
	started   bool
	startedAt time.Time
	// pendingIdent holds a charger's word that arrived before the loadpoint
	// saw the plug-in.
	pendingIdent map[string]pendingIdent
	// vehicleDrivers are the configured drivers that report a car's level,
	// bound to a car before their first reading so an asleep car can be
	// woken.
	vehicleDrivers []string
	// intentMoved is set while a sync hands a loadpoint a goal or surplus
	// choice, so the tick replans.
	intentMoved bool
}

// SetVehicleDrivers names the configured vehicle drivers. Each binds to the
// car with its VIN, or to the only car, at the next tick, without waiting for
// its first reading.
func (r *Runtime) SetVehicleDrivers(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vehicleDrivers = slices.Clone(names)
}

type pendingIdent struct {
	ident string
	at    time.Time
}

// Errors the operations return. appproto maps them onto refusal codes.
var (
	ErrUnknownCar     = errors.New("cars: no such car")
	ErrUnknownCharger = errors.New("cars: no such charger")
	ErrUnplugged      = errors.New("cars: nothing is plugged in")
	ErrInvalid        = errors.New("cars: invalid value")
	// ErrSessionEnded is a tap for a plug-in that has ended.
	ErrSessionEnded = errors.New("cars: that plug-in has ended")
	// ErrDuplicate is a VIN or identifier another car already has.
	ErrDuplicate = errors.New("cars: another car has that VIN or identifier")
	// ErrElsewhere is a tap naming a car another charger has.
	ErrElsewhere = errors.New("cars: that car is on another charger")
	// ErrTooMany is a car past MaxCars.
	ErrTooMany = errors.New("cars: the box keeps no more cars")
)

// MaxCars is how many cars a box keeps, and maxTombstones how many removed
// ids it remembers. The oldest tombstone goes first.
const (
	MaxCars       = 32
	maxTombstones = 64
)

// New builds a runtime. Call Start before the first Tick.
func New(deps Deps) *Runtime {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Runtime{
		deps:         deps,
		sources:      map[string]*sourceState{},
		applied:      map[string]loadpoint.CarBinding{},
		pendingIdent: map[string]pendingIdent{},
	}
}

// OnChange registers fn to run after a session's car, a car's level source,
// goal or choices change: the hook for anything that pushes a plan or a
// charger view. Called outside the runtime's lock.
func (r *Runtime) OnChange(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, fn)
}

// Start loads the cars, or makes them from what the box has on first start,
// and hands each loadpoint its car. It runs after the loadpoints' schedules
// and surplus choices are hydrated, and before the first control tick, so a
// restored session keeps its car's capacity.
func (r *Runtime) Start(profiles []Profile) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	r.mu.Lock()
	r.startedAt = r.deps.Now()
	loaded := false
	if raw, ok := r.deps.Store.LoadConfig(StoreKey); ok && raw != "" {
		var rec record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			// Keep the row for whoever looks, and make the cars again from
			// the loadpoints' own rows, which the box keeps writing.
			slog.Error("cars: the stored cars cannot be read; making them again from the chargers", "err", err)
			_ = r.deps.Store.SaveConfig(StoreKey+":unreadable", raw)
			rec = record{}
		}
		for _, car := range rec.Cars {
			car.Goal.Normalize()
		}
		for _, s := range rec.Sessions {
			s.restored = true
			s.seeded = true
			if s.ID == "" {
				s.ID = newSessionID()
			}
		}
		r.rec = rec
		loaded = rec.Version != 0
	}
	r.ensureMaps()
	if !r.rec.Migrated {
		r.migrateLocked()
		r.mergeOnlyProfileLocked(profiles)
	}
	r.syncProfilesLocked(profiles)
	r.started = true
	var err error
	if !loaded || r.dirty {
		err = r.saveLocked()
	}
	acts := r.syncLocked(r.deps.Now())
	r.intentMoved = false
	r.mu.Unlock()
	run(acts)
	// The goals the sync handed the loadpoints have no deadline until the
	// schedules roll; the planner may run before the first control tick.
	r.deps.Loadpoints.RollSchedules(r.deps.Now().UTC())
	return err
}

func (r *Runtime) ensureMaps() {
	if r.rec.Version == 0 {
		r.rec.Version = 1
	}
	if r.rec.Sessions == nil {
		r.rec.Sessions = map[string]*session{}
	}
	if r.rec.LastCar == nil {
		r.rec.LastCar = map[string]string{}
	}
	if r.rec.Profiles == nil {
		r.rec.Profiles = map[string]string{}
	}
	if r.rec.ProfileSeen == nil {
		r.rec.ProfileSeen = map[string]Profile{}
	}
}

// migrateLocked makes the cars from what the box had before them. Nothing the
// owner set is lost: each loadpoint's capacity, plug-in level, goal and
// surplus choice become a car's. The site's battery rule stays on the
// loadpoint.
func (r *Runtime) migrateLocked() {
	lps := r.deps.Loadpoints
	configs := lps.Configs()
	if len(configs) == 0 {
		return
	}
	r.rec.Migrated = true
	r.dirty = true
	for i, cfg := range configs {
		st, _ := lps.State(cfg.ID)
		car := &Car{
			ID:          newID(),
			BoxMinted:   true,
			Name:        fmt.Sprintf("Car %d", i+1),
			CapacityWh:  cfg.VehicleCapacityWh,
			PluginSoC:   cfg.PluginSoC,
			Goal:        goalOnly(st.Schedule),
			SurplusOnly: st.SurplusOnly,
			UsualLP:     cfg.ID,
		}
		r.rec.Cars = append(r.rec.Cars, car)
		r.rec.LastCar[cfg.ID] = car.ID
		slog.Info("cars: made a car from the loadpoint's settings", "lp", cfg.ID, "car", car.ID, "name", car.Name)
	}
}

// mergeOnlyProfileLocked folds a lone vehicles[] profile into the lone car
// the loadpoint made: one home, one charger, one profile is one car, which
// the charger names by its identifiers and which is also the only car. Its
// name, identifiers and capacity come from the profile; its goal and
// choices stay the ones the charger showed.
func (r *Runtime) mergeOnlyProfileLocked(profiles []Profile) {
	if len(r.rec.Cars) != 1 || len(profiles) != 1 || profiles[0].ID == "" {
		return
	}
	car, p := r.rec.Cars[0], profiles[0]
	if p.Name != "" {
		car.Name = p.Name
	} else {
		car.Name = p.ID
	}
	if p.CapacityWh > 0 {
		car.CapacityWh = p.CapacityWh
	}
	car.Identifiers = append([]string(nil), p.Identifiers...)
	car.ProfileID, car.ProfileTarget = p.ID, p.TargetSoC
	r.rec.Profiles[p.ID] = car.ID
	r.rec.ProfileSeen[p.ID] = p
	r.dirty = true
}

// Profile is a vehicles[] profile from config.yaml.
type Profile struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	CapacityWh  float64  `json:"capacity_wh,omitempty"`
	Identifiers []string `json:"identifiers,omitempty"`
	SurplusOnly bool     `json:"surplus_only,omitempty"`
	TargetSoC   float64  `json:"target_soc,omitempty"`
}

// SettingsChange is a loadpoint setting someone changed in config.yaml, by
// hand or on the box's settings page. For one release such a change goes to
// the car the charger shows, as the charger routes do. A nil field is
// unchanged.
type SettingsChange struct {
	LP          string
	CapacityWh  *float64
	PluginSoC   *float64
	SurplusOnly *bool
}

// Reload follows a config.yaml change: a box that had no loadpoint makes its
// cars from the first one, each vehicles[] profile the box has not seen
// becomes a car, an edited profile changes its car, and a changed loadpoint
// setting becomes its car's.
func (r *Runtime) Reload(profiles []Profile, changes ...SettingsChange) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	r.mu.Lock()
	if !r.rec.Migrated {
		r.migrateLocked()
	}
	r.syncProfilesLocked(profiles)
	for _, c := range changes {
		car, _ := r.viewCarLocked(c.LP)
		if car == nil {
			continue
		}
		if c.CapacityWh != nil && *c.CapacityWh >= 1000 && *c.CapacityWh <= 300000 {
			car.CapacityWh = *c.CapacityWh
		}
		if c.PluginSoC != nil && *c.PluginSoC >= 0 && *c.PluginSoC <= 1 {
			car.PluginSoC = *c.PluginSoC
		}
		if c.SurplusOnly != nil {
			car.SurplusOnly = *c.SurplusOnly
		}
		r.dirty = true
	}
	var acts []func()
	if r.dirty {
		_ = r.saveLocked()
		acts = r.syncLocked(r.deps.Now())
	}
	r.mu.Unlock()
	run(acts)
}

func (r *Runtime) syncProfilesLocked(profiles []Profile) {
	for _, p := range profiles {
		if p.ID == "" {
			continue
		}
		p.Identifiers = slices.Clone(p.Identifiers)
		if carID, seen := r.rec.Profiles[p.ID]; seen {
			prev, known := r.rec.ProfileSeen[p.ID]
			if known && sameProfile(prev, p) {
				continue
			}
			if car := r.carLocked(carID); car != nil && known {
				r.followProfileLocked(car, prev, p)
			}
			r.rec.ProfileSeen[p.ID] = p
			r.dirty = true
			continue
		}
		if len(r.rec.Cars) >= MaxCars {
			slog.Warn("cars: the box keeps no more cars; a vehicle profile made none", "profile", p.ID)
			continue
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		car := &Car{
			ID: newID(), BoxMinted: true, Name: name, CapacityWh: p.CapacityWh,
			Identifiers: append([]string(nil), p.Identifiers...), SurplusOnly: p.SurplusOnly,
			ProfileID: p.ID, ProfileTarget: p.TargetSoC,
		}
		r.rec.Cars = append(r.rec.Cars, car)
		r.rec.Profiles[p.ID] = car.ID
		r.rec.ProfileSeen[p.ID] = p
		r.dirty = true
		slog.Info("cars: made a car from a vehicle profile", "profile", p.ID, "car", car.ID)
	}
}

// followProfileLocked gives a car what changed in its profile, and leaves
// alone what did not, which the owner may have set since.
func (r *Runtime) followProfileLocked(car *Car, prev, p Profile) {
	if p.Name != prev.Name && p.Name != "" {
		car.Name = p.Name
	}
	if p.CapacityWh != prev.CapacityWh && p.CapacityWh >= 1000 && p.CapacityWh <= 300000 {
		car.CapacityWh = p.CapacityWh
	}
	if !slices.Equal(p.Identifiers, prev.Identifiers) {
		next := *car
		next.Identifiers = slices.Clone(p.Identifiers)
		if err := r.duplicateLocked(&next, nil); err != nil {
			slog.Warn("cars: a vehicle profile's identifiers are another car's; its car keeps its own",
				"profile", p.ID, "car", car.ID)
		} else {
			car.Identifiers = next.Identifiers
		}
	}
	if p.SurplusOnly != prev.SurplusOnly {
		car.SurplusOnly = p.SurplusOnly
	}
	if p.TargetSoC != prev.TargetSoC {
		car.ProfileTarget = p.TargetSoC
	}
	slog.Info("cars: a vehicle profile changed; its car follows", "profile", p.ID, "car", car.ID)
}

func sameProfile(a, b Profile) bool {
	return a.Name == b.Name && a.CapacityWh == b.CapacityWh && slices.Equal(a.Identifiers, b.Identifiers) &&
		a.SurplusOnly == b.SurplusOnly && a.TargetSoC == b.TargetSoC
}

// anyIdentifiersLocked reports whether some car lists a charger identifier.
// Until one does, a charger's identifier names nobody.
func (r *Runtime) anyIdentifiersLocked() bool {
	for _, c := range r.rec.Cars {
		if len(c.Identifiers) > 0 {
			return true
		}
	}
	return false
}

// saveLocked writes the record.
func (r *Runtime) saveLocked() error {
	b, err := json.Marshal(r.rec)
	if err != nil {
		return err
	}
	if err := r.deps.Store.SaveConfig(StoreKey, string(b)); err != nil {
		return fmt.Errorf("cars: save: %w", err)
	}
	r.dirty, r.levelSave = false, false
	r.lastSave = r.deps.Now()
	return nil
}

func (r *Runtime) carLocked(id string) *Car {
	if id == "" {
		return nil
	}
	for _, c := range r.rec.Cars {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (r *Runtime) carForSourceLocked(driver string) *Car {
	for _, c := range r.rec.Cars {
		for _, s := range c.Sources {
			if s == driver {
				return c
			}
		}
	}
	return nil
}

// sessionOfLocked is the plugged-in loadpoint whose session has the car.
func (r *Runtime) sessionOfLocked(carID string) (string, *session) {
	for lp, s := range r.rec.Sessions {
		if s.CarID == carID {
			return lp, s
		}
	}
	return "", nil
}

// viewCarLocked is the car a loadpoint's routes act on: the session's car
// while plugged in, else the car it last had, else the only car.
func (r *Runtime) viewCarLocked(lpID string) (car *Car, visitor bool) {
	if s := r.rec.Sessions[lpID]; s != nil {
		if c := r.carLocked(s.CarID); c != nil {
			return c, false
		}
		return nil, true
	}
	if c := r.carLocked(r.rec.LastCar[lpID]); c != nil {
		return c, false
	}
	if len(r.rec.Cars) == 1 {
		return r.rec.Cars[0], false
	}
	return nil, false
}

// sessionForRouteLocked opens the session of a charger plugged in since the
// last tick and asks the identification order at once, so a route acts on
// the car the box can name, or on the session alone, and never on the car
// the charger last had. The loadpoint hears of it at the next tick.
func (r *Runtime) sessionForRouteLocked(lpID string) {
	if r.rec.Sessions[lpID] != nil {
		return
	}
	states := r.deps.Loadpoints.States()
	var st loadpoint.State
	found := false
	for _, x := range states {
		if x.ID == lpID {
			st, found = x, true
		}
	}
	if !found || !st.Observed || !st.PluggedIn {
		return
	}
	now := r.deps.Now()
	s := r.openSessionLocked(st, now)
	r.applyAnswerLocked(lpID, s, r.answerLocked(st, s, states, now), now)
}

func (r *Runtime) notify() {
	r.mu.Lock()
	listeners := append([]func(){}, r.listeners...)
	r.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

// applyAndReplan hands the loadpoints what changed between two replan
// requests. The first marks the plan pending before any charger shows the
// change, so a phone that reads the plan in between sees it updating, never
// a new car beside the old plan as if that were current. The second, after
// the schedules roll, plans with the change and supersedes the first.
func (r *Runtime) applyAndReplan(acts []func(), reason string) {
	if len(acts) > 0 && r.deps.Replan != nil {
		r.deps.Replan(reason)
	}
	run(acts)
	r.replan(reason)
}

// replan rolls the loadpoints' schedules and asks the planner to plan again.
// A goal a car brings clears the loadpoint's rolled deadline until the
// schedules roll, and a replan before that plans without the car.
func (r *Runtime) replan(reason string) {
	r.deps.Loadpoints.RollSchedules(r.deps.Now().UTC())
	if r.deps.Replan != nil {
		r.deps.Replan(reason)
	}
}

func run(acts []func()) {
	for _, a := range acts {
		a()
	}
}

// newID mints a car id: 128 random bits as 32 lowercase hex digits.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// validID accepts an id an owner's app minted on another box.
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// normVIN returns s as a VIN, or "" when it is not shaped like one: 17
// characters, digits and capitals, never I, O or Q.
func normVIN(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 17 {
		return ""
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'O' && c != 'Q':
		default:
			return ""
		}
	}
	return s
}

// goalOnly is a schedule without the site's battery rule. A schedule with no
// target is no goal, whatever clock it carries.
func goalOnly(s loadpoint.Schedule) loadpoint.Schedule {
	if !s.HasTarget() {
		return loadpoint.Schedule{}
	}
	s.SurplusUnlockBatSoC = 0
	return s
}

// sameGoal compares what the owner chose, not what Core derived from it.
func sameGoal(a, b loadpoint.Schedule) bool {
	a, b = goalOnly(a), goalOnly(b)
	if !a.HasTarget() || !b.HasTarget() {
		return a.HasTarget() == b.HasTarget()
	}
	ac, aok := a.TimeOfDayMin()
	bc, bok := b.TimeOfDayMin()
	if !aok {
		ac = a.TimeOfDayMinUTC
	}
	if !bok {
		bc = b.TimeOfDayMinUTC
	}
	return a.SoC == b.SoC && a.Recurring == b.Recurring && a.Days == b.Days &&
		a.FinishAtVehicleLimit == b.FinishAtVehicleLimit && ac == bc
}

// emptyGoal reports a goal with nothing in it, the site rule aside.
func emptyGoal(s loadpoint.Schedule) bool { return goalOnly(s).Empty() }

var identifiedRank = map[string]int{
	protocol.CarIdentifiedByCharger: 0,
	protocol.CarIdentifiedByCar:     1,
	protocol.CarIdentifiedByOnly:    2,
	protocol.CarIdentifiedByTap:     3,
	protocol.CarIdentifiedByAsking:  4,
	protocol.CarIdentifiedByNone:    5,
}

// newSessionID names one plug-in: 64 random bits as hex.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
