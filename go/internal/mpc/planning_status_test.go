package mpc

import (
	"context"
	"strings"
	"testing"
)

func TestReplanLimitsChargerStepsAndPublishesWarning(t *testing.T) {
	for _, probe := range []bool{false, true} {
		svc := newCancellationTestService(t, nil)
		lp := validPlanningLoadpoint()
		lp.ID = "Easee"
		lp.AllowedStepsW = []float64{0, 4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}
		if probe {
			svc.Loadpoints = func(int) []*LoadpointSpec { return []*LoadpointSpec{lp} }
		} else {
			svc.Defaults.Loadpoints, svc.Defaults.Loadpoint = []*LoadpointSpec{lp}, lp
		}
		plan := svc.Replan(context.Background())
		snapshot := svc.PlanSnapshot()
		if plan == nil || snapshot.Outdated || snapshot.Error != "" {
			t.Fatalf("plan unavailable: %+v", snapshot)
		}
		if len(snapshot.Warnings) != 1 || !strings.Contains(snapshot.Warnings[0], "11000 W") {
			t.Fatal(snapshot.Warnings)
		}
		if lp.AllowedStepsW[len(lp.AllowedStepsW)-1] != 11100 {
			t.Fatal("planning changed the caller's spec")
		}
		svc.mu.RLock()
		steps := append([]float64(nil), svc.lastParams.Loadpoints[0].AllowedStepsW...)
		svc.mu.RUnlock()
		for _, step := range steps {
			if step > 11000 {
				t.Fatalf("solver received step %v", step)
			}
		}
		for _, action := range plan.Actions {
			if action.LoadpointW > 11000 {
				t.Fatalf("planned %v W above max", action.LoadpointW)
			}
		}
	}
}

func TestPlanningErrorReportsCauseAndClearsOnRecovery(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	svc.Defaults.MaxDischargeW = -1
	svc.Replan(context.Background())
	failed := svc.PlanSnapshot()
	if failed.Pending || !strings.Contains(failed.Error, "max_discharge_w") {
		t.Fatalf("missing actual cause: %+v", failed)
	}
	svc.Defaults.MaxDischargeW = 3000
	svc.Replan(context.Background())
	if got := svc.PlanSnapshot(); got.Error != "" || got.Outdated || got.Plan == nil {
		t.Fatalf("recovery did not clear failure: %+v", got)
	}
}

func TestSupersededPlanningFailureCannotReplaceCurrentStatus(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	old := svc.beginReplan(context.Background(), "old")
	newer := svc.beginReplan(context.Background(), "newer")
	svc.failedReplan(newer, "current failure")
	svc.failedReplan(old, "old failure")
	svc.finishReplan(old)
	svc.finishReplan(newer)
	if got := svc.PlanSnapshot(); got.Error != "current failure" {
		t.Fatalf("superseded status won: %+v", got)
	}
}
