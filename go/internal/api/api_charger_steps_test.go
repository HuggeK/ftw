package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestPostConfigSavesLimitedStepsAndReturnsWarning(t *testing.T) {
	srv, _, cfg := postConfigServer(t, nil)
	srv.deps.SaveConfig = config.SaveAtomic
	body := strings.TrimSpace(firstSiteMeterConfig)
	body = body[:len(body)-1] + `,"loadpoints":[{"id":"Easee","min_charge_w":4800,"max_charge_w":11000,"allowed_steps_w":[0,4800,10400,11100]}]}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("save rejected recoverable config: %s", rr.Body.String())
	}
	var response struct {
		Warnings   []string
		Loadpoints []config.Loadpoint
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Warnings) != 1 || len(response.Loadpoints) != 1 {
		t.Fatalf("missing save feedback: %s", rr.Body.String())
	}
	if response.Loadpoints[0].AllowedStepsW[3] != 11000 {
		t.Fatal("save response did not return the corrected steps")
	}
	if got := cfg.Loadpoints[0].AllowedStepsW[3]; got != 11000 {
		t.Fatalf("live config step=%v", got)
	}
	saved, err := config.Load(srv.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Loadpoints[0].AllowedStepsW[3] != 11000 {
		t.Fatal("invalid step persisted")
	}
	srv.deps.MPC = &mpc.Service{}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mpc/plan", nil))
	if !strings.Contains(rr.Body.String(), "11000 W") {
		t.Fatalf("Plan lost correction warning: %s", rr.Body.String())
	}
}

func TestMPCPlanReturnsActualPlanningFailure(t *testing.T) {
	svc := &mpc.Service{SiteEconomics: mpc.SiteEconomics{FuseMaxW: -1}}
	svc.Replan(context.Background())
	rr := httptest.NewRecorder()
	New(&Deps{MPC: svc}).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mpc/plan", nil))
	var response struct {
		Meta struct{ Error string }
		Plan *mpc.Plan
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Plan != nil || !strings.Contains(response.Meta.Error, "fuse_max_w") {
		t.Fatalf("actual failure missing: %s", rr.Body.String())
	}
}
