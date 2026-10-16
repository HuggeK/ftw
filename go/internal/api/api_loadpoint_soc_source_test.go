package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol"
)

// fakeCars answers Session from a table, as the cars runtime would.
type fakeCars map[string]cars.SessionView

func (f fakeCars) Cars() []protocol.Car      { return nil }
func (f fakeCars) Removed() []cars.Tombstone { return nil }
func (f fakeCars) Session(lp string) (cars.SessionView, bool) {
	v, ok := f[lp]
	return v, ok
}
func (f fakeCars) SetViewCarCapacity(string, float64) error { return nil }

// The manager reports "completed" when the completion latch pinned the
// estimate to the target. The decoration used to overwrite that with
// "inferred" on every site without a vehicle driver, so the modal could
// never say why the slider sat where it sat (#1059).
func TestDecorateLoadpointsKeepsCompletedSoCSource(t *testing.T) {
	states := []loadpoint.State{
		{ID: "a", PluggedIn: true, SoCSource: "completed"},
		{ID: "b", PluggedIn: true},
		{ID: "c", PluggedIn: false},
	}
	decorateLoadpointsWithCar(states, fakeCars{}, time.Now())
	if states[0].SoCSource != "completed" {
		t.Errorf("a: SoCSource = %q, want completed kept", states[0].SoCSource)
	}
	if states[1].SoCSource != "inferred" {
		t.Errorf("b: SoCSource = %q, want inferred", states[1].SoCSource)
	}
	if states[2].SoCSource != "" {
		t.Errorf("c: unplugged should stay unattributed, got %q", states[2].SoCSource)
	}
}

// Fredrik's box on 15 October: the Tesla read 14 % nine minutes ago and
// nothing has flowed since. Before cars the reading aged out after five
// minutes and the level showed as "inferred". It is still the car's level.
func TestDecorateLoadpointsKeepsAnOldReadingWithNoEnergySinceFromTheCar(t *testing.T) {
	now := time.Date(2026, 10, 15, 16, 40, 0, 0, time.UTC)
	read := now.Add(-9 * time.Minute)
	views := fakeCars{"easee": {CarID: "c1", Source: protocol.CarSoCVehicle, At: read, SoC: .14, Shown: .14, ChargeLimit: .8,
		Driver: "tesla-ha", ChargingState: "Stopped", DriverAt: read}}
	states := []loadpoint.State{{ID: "easee", PluggedIn: true, CurrentSoC: .14}}
	decorateLoadpointsWithCar(states, views, now)
	got := states[0]
	if got.SoCSource != "vehicle" || got.VehicleStale || got.VehicleDriver != "tesla-ha" || got.VehicleSoC != .14 ||
		got.VehicleSoCAgeS != 540 || got.VehicleChargeLimit != .8 || got.CurrentSoC != .14 {
		t.Fatalf("a nine-minute-old reading with nothing delivered since: %+v", got)
	}
}

// Past the box's limit of energy since the reading, the same level is an
// estimate. The controller's level is never overwritten by the display.
func TestDecorateLoadpointsShowsAnEstimatePastTheEnergyLimit(t *testing.T) {
	now := time.Now()
	views := fakeCars{"easee": {CarID: "c1", Source: protocol.CarSoCVehicle, Estimate: true, At: now.Add(-time.Hour),
		SoC: .14, Shown: .22, Driver: "tesla-ha"}}
	states := []loadpoint.State{{ID: "easee", PluggedIn: true, CurrentSoC: .22}}
	decorateLoadpointsWithCar(states, views, now)
	if got := states[0]; got.SoCSource != "inferred" || !got.VehicleStale || got.CurrentSoC != .22 || got.VehicleSoC != .14 {
		t.Fatalf("estimate: %+v", got)
	}
}

// While a reading is exact, the level shown and planned from is the car's own
// number, not the loadpoint's estimate of 0.9 kWh more.
func TestDecorateLoadpointsShowsTheCarsOwnNumberWhileExact(t *testing.T) {
	views := fakeCars{"easee": {CarID: "c1", Source: protocol.CarSoCVehicle, At: time.Now(), SoC: .14, Shown: .14}}
	states := []loadpoint.State{{ID: "easee", PluggedIn: true, CurrentSoC: .151}}
	decorateLoadpointsWithCar(states, views, time.Now())
	if got := states[0]; got.CurrentSoC != .14 || got.SoCSource != "vehicle" {
		t.Fatalf("exact: %+v", got)
	}
}

// A visitor's car has no level, and the route leaves current_soc out
// rather than showing the energy delivered as one.
func TestLoadpointRouteLeavesAVisitorsLevelOut(t *testing.T) {
	states := []loadpoint.State{{ID: "easee", PluggedIn: true, SoCUnknown: true, SoCSource: "unknown", CurrentSoC: .05}}
	decorateLoadpointsWithCar(states, fakeCars{}, time.Now())
	if states[0].SoCSource != "unknown" {
		t.Fatalf("visitor source = %q", states[0].SoCSource)
	}
	s := &Server{deps: &Deps{}}
	view := s.loadpointsWithFeedback(states)[0]
	if view.Level != nil {
		t.Fatalf("visitor level = %v, want none", *view.Level)
	}
}

// recordingCars is fakeCars that keeps what the routes asked of it.
type recordingCars struct {
	fakeCars
	cars     []protocol.Car
	capacity map[string]float64
}

func (r *recordingCars) Cars() []protocol.Car {
	return append([]protocol.Car(nil), r.cars...)
}
func (r *recordingCars) SetViewCarCapacity(lp string, wh float64) error {
	r.capacity[lp] = wh
	return nil
}

// GET /api/cars lists the cars in the registry's names spelled in
// snake_case, as every route spells them.
func TestCarsRouteListsTheCars(t *testing.T) {
	soc := int64(140)
	views := &recordingCars{cars: []protocol.Car{{ID: "c1", Name: "Car 1", VIN: "XP7YGCEK7SB736799", CapacityWh: 76000, SocPermille: &soc,
		SocSource: protocol.CarSoCVehicle, Goal: &protocol.CarGoal{TargetPermille: 800, ReadyByMin: 420, Days: 0x7F},
		Sources: []string{"tesla-ha"}}}}
	srv := New(&Deps{Cars: views})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/cars", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Cars []map[string]any `json:"cars"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Cars) != 1 {
		t.Fatalf("body %s: %v", rr.Body.String(), err)
	}
	c := got.Cars[0]
	goal, _ := c["goal"].(map[string]any)
	if c["soc_permille"] != 140.0 || c["soc_source"] != "vehicle" || c["capacity_wh"] != 76000.0 || goal["ready_by_min"] != 420.0 ||
		c["vin"] != "XP7YGCEK7SB736799" {
		t.Fatalf("an owner's car %v", c)
	}

	// A viewer sees the level and the goal, and nothing that says which car.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cars", nil)
	req = req.WithContext(apiauth.WithCaller(req.Context(), apiauth.Caller{Role: apiauth.RoleViewer,
		Scopes: apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleViewer]...)}))
	srv.Handler().ServeHTTP(rr, req)
	got.Cars = nil
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Cars) != 1 {
		t.Fatalf("viewer body %s: %v", rr.Body.String(), err)
	}
	c = got.Cars[0]
	if c["soc_permille"] != 140.0 || c["name"] != "Car 1" || c["vin"] != nil || c["sources"] != nil || c["capacity_wh"] != 0.0 {
		t.Fatalf("a viewer's car %v", c)
	}
}

// For one release the battery size route sets the size of the car the
// charger shows, and still writes the config so a rollback finds it.
func TestTheBatterySizeRouteSetsTheCarsCapacity(t *testing.T) {
	srv, _ := vehicleServer(t)
	views := &recordingCars{capacity: map[string]float64{}}
	srv.deps.Cars = views
	if rr := postVehicle(srv, "garage", `{"capacity_wh":77400}`); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if views.capacity["garage"] != 77400 || srv.deps.Cfg.Loadpoints[0].VehicleCapacityWh != 77400 {
		t.Fatalf("car %v, config %v", views.capacity, srv.deps.Cfg.Loadpoints[0].VehicleCapacityWh)
	}
}
