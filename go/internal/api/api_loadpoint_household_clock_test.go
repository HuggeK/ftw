package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// time_of_day_min is the ready time on the household clock, in the box's
// zone. Europe/Stockholm leaves summer time on 2026-10-25 and returns on
// 2027-03-28; 07:00 is 05:00 UTC in summer and 06:00 UTC in winter.

type wireSchedule struct {
	TimeOfDayMin    *int `json:"time_of_day_min"`
	TimeOfDayMinUTC int  `json:"time_of_day_min_utc"`
}

// householdServer is a schedule server in Stockholm at *now.
func householdServer(t *testing.T, now *time.Time) *Server {
	t.Helper()
	srv, mgr, _ := newScheduleServer(t)
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetLocation(loc)
	mgr.SetNowFn(func() time.Time { return *now })
	return srv
}

func getSchedule(t *testing.T, srv *Server) wireSchedule {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
	var body struct {
		Loadpoints []struct {
			Schedule wireSchedule `json:"schedule"`
		} `json:"loadpoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Loadpoints) != 1 {
		t.Fatalf("GET /api/loadpoints: %v: %s", err, rr.Body.String())
	}
	return body.Loadpoints[0].Schedule
}

func checkSchedule(t *testing.T, got wireSchedule, clock, minUTC int) {
	t.Helper()
	if got.TimeOfDayMin == nil || *got.TimeOfDayMin != clock || got.TimeOfDayMinUTC != minUTC {
		t.Fatalf("schedule = %+v (time_of_day_min %v), want time_of_day_min %d and time_of_day_min_utc %d",
			got, got.TimeOfDayMin, clock, minUTC)
	}
}

func postTarget(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/target", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestHouseholdMinuteRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	srv := householdServer(t, &now)
	if rr := putSchedule(t, srv, "garage", `{"soc":0.8,"time_of_day_min":420,"recurring":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	checkSchedule(t, getSchedule(t, srv), 420, 300)
}

func TestHouseholdMinuteWinsOverUTCMinute(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	srv := householdServer(t, &now)
	// The UTC minute says 13:00 in Stockholm. The household minute wins.
	if rr := putSchedule(t, srv, "garage", `{"soc":0.8,"time_of_day_min":420,"time_of_day_min_utc":660,"recurring":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	checkSchedule(t, getSchedule(t, srv), 420, 300)
	if rr := postTarget(t, srv, `{"schedule":{"soc":0.8,"time_of_day_min":0,"time_of_day_min_utc":300}}`); rr.Code != http.StatusOK {
		t.Fatalf("POST target: %d %s", rr.Code, rr.Body.String())
	}
	checkSchedule(t, getSchedule(t, srv), 0, 22*60)
}

// A client that sends only time_of_day_min_utc is read as before: that
// UTC minute on today's date, answered with the same UTC minute.
func TestOldClientsSendAndReadTheUTCMinute(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	srv := householdServer(t, &now)
	if rr := putSchedule(t, srv, "garage", `{"soc":0.8,"time_of_day_min_utc":300,"recurring":true}`); rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	checkSchedule(t, getSchedule(t, srv), 420, 300)
	if rr := postTarget(t, srv, `{"schedule":{"soc":0.8,"time_of_day_min_utc":360}}`); rr.Code != http.StatusOK {
		t.Fatalf("POST target: %d %s", rr.Code, rr.Body.String())
	}
	checkSchedule(t, getSchedule(t, srv), 480, 360)
}

// The household minute stays put across both changes; only the UTC
// minute that older clients read moves.
func TestHouseholdMinuteOverStockholmDSTChanges(t *testing.T) {
	for _, c := range []struct {
		name   string
		saved  time.Time
		before time.Time
		after  time.Time
		utc    [2]int
	}{
		{"autumn 2026-10-25", time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC), [2]int{300, 360}},
		{"spring 2027-03-28", time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC),
			time.Date(2027, 3, 27, 10, 0, 0, 0, time.UTC), time.Date(2027, 3, 29, 10, 0, 0, 0, time.UTC), [2]int{360, 300}},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := c.saved
			srv := householdServer(t, &now)
			if rr := putSchedule(t, srv, "garage", `{"soc":0.8,"time_of_day_min":420,"recurring":true}`); rr.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
			}
			now = c.before
			checkSchedule(t, getSchedule(t, srv), 420, c.utc[0])
			now = c.after
			checkSchedule(t, getSchedule(t, srv), 420, c.utc[1])
		})
	}
}
