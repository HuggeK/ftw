package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/protocol"
)

// The runtime is what main.go hands the routes.
var _ CarEdits = (*cars.Runtime)(nil)

type carRows struct {
	mu   sync.Mutex
	rows map[string]string
}

func (k *carRows) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.rows[key]
	return v, ok
}

func (k *carRows) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows[key] = value
	return nil
}

// carsServer is a box with one charger and the cars runtime, as main.go
// wires them; on first start the box makes Car 1 from the charger.
func carsServer(t *testing.T) (*Server, *loadpoint.Manager, *cars.Runtime) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", VehicleCapacityWh: 60000, MaxChargeW: 11000}})
	rt := cars.New(cars.Deps{Store: &carRows{rows: map[string]string{}}, Loadpoints: mgr})
	if err := rt.Start(nil); err != nil {
		t.Fatal(err)
	}
	return New(&Deps{Cars: rt, Loadpoints: mgr}), mgr, rt
}

func carRequest(srv *Server, method, path, body string) (int, map[string]any) {
	rr := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const (
	carA = "0123456789abcdef0123456789abcdef"
	carB = "fedcba9876543210fedcba9876543210"
)

// The box page keeps the owner's list as the owner's app does: add, rename,
// resize and remove, with the ops' refusals.
func TestTheBoxPageKeepsTheCars(t *testing.T) {
	srv, _, rt := carsServer(t)

	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carA+`","name":"Volvo","capacity_wh":69000,"vin":"YV1AB0CD9EF123456"}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars",
		`{"id":"`+carB+`","name":"Kia","capacity_wh":64000,"vin":"YV1AB0CD9EF123456"}`); code != http.StatusConflict ||
		body["code"] != protocol.ErrDuplicate || body["reason"] != "duplicate" {
		t.Fatalf("a VIN another car has: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `{"id":"short","name":"Kia"}`); code != http.StatusBadRequest ||
		body["reason"] != "invalid" {
		t.Fatalf("a bad id: %d %v", code, body)
	}

	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"name":"Family Volvo","capacity_wh":70000}`); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, body)
	}
	got, err := rt.Car(carA)
	if err != nil || got.Name != "Family Volvo" || got.CapacityWh != 70000 || got.VIN != "YV1AB0CD9EF123456" {
		t.Fatalf("after the edit: %+v %v", got, err)
	}
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carA, `{"capacity_wh":10}`); code != http.StatusBadRequest {
		t.Fatalf("a capacity the box refuses: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPatch, "/api/cars/"+carB, `{"name":"Kia"}`); code != http.StatusNotFound ||
		body["reason"] != "car" {
		t.Fatalf("an unknown car: %d %v", code, body)
	}

	if code, body := carRequest(srv, http.MethodDelete, "/api/cars/"+carA, ""); code != http.StatusOK {
		t.Fatalf("remove: %d %v", code, body)
	}
	if code, _ := carRequest(srv, http.MethodDelete, "/api/cars/"+carA, ""); code != http.StatusNotFound {
		t.Fatalf("removed twice: %d", code)
	}
	_, list := carRequest(srv, http.MethodGet, "/api/cars", "")
	removed, _ := list["removed"].([]any)
	if cs, _ := list["cars"].([]any); len(cs) != 1 || len(removed) != 1 {
		t.Fatalf("after the remove: %v", list)
	}
}

// A viewer may see the cars but not keep them.
func TestOnlyAnOwnerKeepsTheCars(t *testing.T) {
	srv, _, _ := carsServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cars", strings.NewReader(`{"id":"`+carA+`","name":"Volvo"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(apiauth.WithCaller(req.Context(), apiauth.Caller{Role: apiauth.RoleViewer,
		Scopes: apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleViewer]...)}))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a viewer added a car: %d %s", rr.Code, rr.Body.String())
	}
}

// The one tap from the box page names the plug-in it answers, and the box
// refuses a tap for one that has ended.
func TestTheBoxPageTapsWhichCar(t *testing.T) {
	srv, mgr, rt := carsServer(t)
	if code, body := carRequest(srv, http.MethodPost, "/api/cars", `{"id":"`+carA+`","name":"Volvo","capacity_wh":69000}`); code != http.StatusOK {
		t.Fatalf("add: %d %v", code, body)
	}
	mgr.ObserveSession("garage", true, 0, 0, true, "easee:EH1", "s1")
	rt.Tick()
	session := rt.ChargerCar("garage").Session
	if session == "" {
		t.Fatal("no session after the plug-in")
	}

	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`"}`); code != http.StatusBadRequest {
		t.Fatalf("a tap that names no car: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car",
		`{"session":"old","car_id":"`+carA+`"}`); code != http.StatusConflict || body["reason"] != "session" {
		t.Fatalf("a tap for an ended plug-in: %d %v", code, body)
	}
	code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`","car_id":"`+carA+`"}`)
	charger, _ := body["charger"].(map[string]any)
	if code != http.StatusOK || charger["car_id"] != carA || charger["car_identified_by"] != protocol.CarIdentifiedByTap {
		t.Fatalf("the tap: %d %v", code, body)
	}
	if st, _ := mgr.State("garage"); st.CarID != carA || st.VehicleCapacityWh != 69000 {
		t.Fatalf("the charger does not plan for the Volvo: %+v", st)
	}
	// None of these: a visitor's.
	code, body = carRequest(srv, http.MethodPost, "/api/loadpoints/garage/car", `{"session":"`+session+`","car_id":""}`)
	charger, _ = body["charger"].(map[string]any)
	if code != http.StatusOK || charger["car_id"] != nil || charger["car_identified_by"] != protocol.CarIdentifiedByNone {
		t.Fatalf("none of these: %d %v", code, body)
	}
	if code, body := carRequest(srv, http.MethodPost, "/api/loadpoints/nowhere/car", `{"session":"s","car_id":""}`); code != http.StatusNotFound {
		t.Fatalf("an unknown charger: %d %v", code, body)
	}
}

// The car routes are priced like the ops: keeping the list is the owner's
// configuration with no step-up, and the tap is actuation a phone sends as
// session.car.set.
func TestTheCarRoutesArePricedLikeTheOps(t *testing.T) {
	srv, _, _ := carsServer(t)
	for _, tc := range []struct {
		method, path string
		tier         apiauth.Tier
		op           string
	}{
		{http.MethodPost, "/api/cars", apiauth.TierConfigure, ""},
		{http.MethodPatch, "/api/cars/" + carA, apiauth.TierConfigure, ""},
		{http.MethodDelete, "/api/cars/" + carA, apiauth.TierConfigure, ""},
		{http.MethodPost, "/api/loadpoints/garage/car", apiauth.TierActuate, protocol.OpSessionCarSet},
	} {
		facts := srv.Route(httptest.NewRequest(tc.method, tc.path, nil))
		if facts.Tier != tc.tier || facts.CmdOp != tc.op || (tc.tier == apiauth.TierConfigure && !facts.NoStepUp) {
			t.Errorf("%s %s: %+v", tc.method, tc.path, facts)
		}
	}
}
