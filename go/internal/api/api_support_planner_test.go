package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func plannerSupportFixture(t *testing.T) (*Server, *mpc.Service) {
	t.Helper()
	return plannerSupportFixtureAt(t, filepath.Join(t.TempDir(), "state.db"))
}

func plannerSupportFixtureAt(t *testing.T, database string) (*Server, *mpc.Service) {
	t.Helper()
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now().UTC().Truncate(15 * time.Minute)
	for i := range 4 {
		if err := store.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli()}}); err != nil {
			t.Fatal(err)
		}
	}
	planner := mpc.New(store, nil, "SE3", mpc.Params{Mode: mpc.ModeSelfConsumption,
		CapacityWh: 15000, InitialSoC: .856, SoCMin: .1, SoCMax: .9, SoCLevels: 11, ActionLevels: 5,
		MaxChargeW: 10000, MaxDischargeW: 10000, ChargeEfficiency: .95, DischargeEfficiency: .95})
	planner.BaseLoad = 800
	manager := loadpoint.NewManager()
	manager.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 86500, MaxChargeW: 11000}})
	manager.ObserveSession("garage", true, 0, 2086, true, "charger:TEST", "connection-1")
	manager.SetCurrentSoC("garage", .52)
	manager.SetTarget("garage", .8, now.Add(12*time.Hour))
	return New(&Deps{Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: telemetry.NewStore(), LogRing: telemetry.NewLogRing(), State: store,
		MPC: planner, Loadpoints: manager, Version: "test"}), planner
}

func readSupportFiles(t *testing.T, server *Server) map[string][]byte {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/support/dump", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("support export: %d %s", recorder.Code, recorder.Body.String())
	}
	z, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[path.Base(f.Name)] = body
	}
	return out
}

func TestSupportDumpIncludesFailedPlannerInputs(t *testing.T) {
	server, planner := plannerSupportFixture(t)
	accepted := planner.Replan(context.Background())
	if accepted == nil {
		t.Fatal("no valid baseline")
	}
	planner.Defaults.ChargeEfficiency = math.NaN()
	planner.Replan(context.Background())
	files := readSupportFiles(t, server)
	var document struct {
		Status struct {
			Outdated bool `json:"outdated"`
		} `json:"status"`
		LastAccepted struct {
			DecisionID string `json:"decision_id"`
		} `json:"last_accepted_plan"`
		Failure struct {
			Stage  string `json:"stage"`
			Error  string `json:"error"`
			Inputs struct {
				Params map[string]any   `json:"params"`
				Slots  []map[string]any `json:"slots"`
			} `json:"inputs"`
		} `json:"last_failure"`
	}
	if err := json.Unmarshal(files["planner.json"], &document); err != nil {
		t.Fatalf("failed attempt is absent or unreadable: %v", err)
	}
	if !document.Status.Outdated || document.LastAccepted.DecisionID != accepted.DecisionID || document.Failure.Error == "" {
		t.Fatalf("last accepted plan and failed attempt were confused: %+v", document)
	}
	if document.Failure.Inputs.Params["ChargeEfficiency"] != "NaN" || len(document.Failure.Inputs.Slots) != 4 {
		t.Fatalf("invalid value or exact requested horizon was lost: %+v", document.Failure)
	}
	var points struct {
		Loadpoints []struct {
			PlanningSoC float64 `json:"planning_soc"`
			TargetSoC   float64 `json:"target_soc"`
		} `json:"loadpoints"`
	}
	if err := json.Unmarshal(files["loadpoints.json"], &points); err != nil || len(points.Loadpoints) != 1 {
		t.Fatalf("loadpoint state missing: %v %s", err, files["loadpoints.json"])
	}
	if points.Loadpoints[0].PlanningSoC != .52 || points.Loadpoints[0].TargetSoC != .8 {
		t.Fatalf("export lost the planning level or goal: %+v", points)
	}
	for _, name := range []string{"planner.json", "loadpoints.json"} {
		if !bytes.Contains(files["manifest.json"], []byte(name)) {
			t.Fatalf("manifest does not list %s", name)
		}
	}
}

type supportFailingOptimizer struct{}

func (supportFailingOptimizer) Optimize(context.Context, []mpc.Slot, mpc.Params) (mpc.Plan, error) {
	return mpc.Plan{}, errors.New(`infeasible at 192.0.2.81; {"access_token":"support-token-sentinel"}; Bearer support-bearer-sentinel`)
}
func (supportFailingOptimizer) Close() error { return nil }

func TestSupportDumpKeepsRecoveredFailureAndRedactsEveryFile(t *testing.T) {
	server, planner := plannerSupportFixture(t)
	planner.Optimizer = supportFailingOptimizer{}
	plan := planner.Replan(context.Background())
	if plan == nil || planner.PlanSnapshot().Outdated {
		t.Fatal("fixture did not recover")
	}
	files := readSupportFiles(t, server)
	for name, body := range files {
		for _, secret := range []string{"support-token-sentinel", "support-bearer-sentinel", "192.0.2.81"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s leaks %s", name, secret)
			}
		}
	}
	var document struct {
		Failure struct {
			RecoveredDecisionID string `json:"recovered_decision_id"`
			Error               string `json:"error"`
		} `json:"last_failure"`
	}
	if err := json.Unmarshal(files["planner.json"], &document); err != nil || document.Failure.RecoveredDecisionID != plan.DecisionID || !strings.Contains(document.Failure.Error, "infeasible") {
		t.Fatalf("recovered failure was discarded or paired with the wrong plan: %+v %v", document, err)
	}
}

func TestSupportJSONPreservesLargeIntegersAndRedactsNestedPayloads(t *testing.T) {
	raw := supportJSON(json.RawMessage(`{"params":{"ActionLevels":9223372036854775807},"worker":{"error":{"access_token":"nested-token-sentinel","message":"Bearer nested-bearer-sentinel at 192.0.2.88"}}}`))
	if !json.Valid(raw) || !bytes.Contains(raw, []byte("9223372036854775807")) {
		t.Fatalf("diagnostic export rounded the value that could cause an allocation overflow: %s", raw)
	}
	for _, secret := range []string{"nested-token-sentinel", "nested-bearer-sentinel", "192.0.2.88"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("nested payload leaked %s", secret)
		}
	}
}

func TestSupportDumpBeforeAnyAcceptedPlanIsExplicit(t *testing.T) {
	server, _ := plannerSupportFixture(t)
	files := readSupportFiles(t, server)
	var document map[string]any
	if err := json.Unmarshal(files["planner.json"], &document); err != nil {
		t.Fatal(err)
	}
	if document["enabled"] != true || document["last_accepted_plan"] != nil || document["last_failure"] != nil {
		t.Fatalf("missing evidence was confused with a captured plan or failure: %s", files["planner.json"])
	}
}

func TestPlannerFailureCrashHelper(t *testing.T) {
	database := os.Getenv("FTW_PLANNER_FAILURE_CRASH_DB")
	if database == "" {
		t.Skip("subprocess helper")
	}
	_, planner := plannerSupportFixtureAt(t, database)
	planner.Defaults.ChargeEfficiency = math.NaN()
	if planner.ReplanWithReason(context.Background(), os.Getenv("FTW_PLANNER_FAILURE_CRASH_REASON")) != nil {
		t.Fatal("invalid inputs produced a plan")
	}
	fmt.Println("PLANNER_FAILURE_COMMITTED")
	<-time.After(time.Minute)
}

func TestSupportDumpRestoresFailureAfterTwoProcessKills(t *testing.T) {
	database := filepath.Join(t.TempDir(), "state.db")
	for round := 1; round <= 2; round++ {
		reason := fmt.Sprintf("outage-%d", round)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlannerFailureCrashHelper$")
		cmd.Env = append(os.Environ(), "FTW_PLANNER_FAILURE_CRASH_DB="+database, "FTW_PLANNER_FAILURE_CRASH_REASON="+reason)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		ready := false
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			if lines.Text() == "PLANNER_FAILURE_COMMITTED" {
				ready = true
				break
			}
		}
		if !ready {
			_ = cmd.Wait()
			t.Fatalf("helper did not finish a failed attempt: %v %v %s", ctx.Err(), lines.Err(), stderr.String())
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatal("helper exited cleanly")
		}
		cancel()

		func() {
			store, err := state.Open(database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			planner := mpc.New(store, nil, "SE3", mpc.Params{})
			server := New(&Deps{State: store, MPC: planner, Tel: telemetry.NewStore(), LogRing: telemetry.NewLogRing(), Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{}})
			files := readSupportFiles(t, server)
			var document struct {
				Failure      *mpc.PlanningFailure       `json:"last_failure"`
				LastAccepted json.RawMessage            `json:"last_accepted_plan"`
				Storage      mpc.PlanningFailureStorage `json:"failure_storage"`
			}
			if err := json.Unmarshal(files["planner.json"], &document); err != nil || document.Failure == nil {
				t.Fatalf("failure disappeared after process kill %d: %s %v", round, files["planner.json"], err)
			}
			var inputs struct {
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(document.Failure.Inputs, &inputs); err != nil {
				t.Fatal(err)
			}
			if document.Failure.Trigger != reason || inputs.Params["ChargeEfficiency"] != "NaN" || document.Failure.CapturedAtMs <= 0 {
				t.Fatalf("latest failed inputs were lost after kill %d: %+v", round, document.Failure)
			}
			if !document.Storage.Durable || !document.Storage.Restored || document.Storage.Error != "" {
				t.Fatalf("durable restored evidence has the wrong status: %+v", document.Storage)
			}
			if string(document.LastAccepted) != "null" || planner.Latest() != nil || planner.Diagnose() != nil {
				t.Fatal("failure evidence became an accepted plan after restart")
			}
			if _, ok := planner.SlotDirectiveAt(time.Now()); ok {
				t.Fatal("failure evidence authorized dispatch after restart")
			}
		}()
	}
}

func TestPlannerFailureFailedWriteKeepsPreviousDurableEvidence(t *testing.T) {
	database := filepath.Join(t.TempDir(), "state.db")
	_, planner := plannerSupportFixtureAt(t, database)
	planner.Defaults.ChargeEfficiency = math.NaN()
	planner.ReplanWithReason(context.Background(), "last-durable-attempt")
	if !planner.PlanningFailureStorageSnapshot().Durable {
		t.Fatal("initial failure was not durable")
	}
	_ = planner.Store.Close()
	planner.ReplanWithReason(context.Background(), "failed-storage-attempt")
	if status := planner.PlanningFailureStorageSnapshot(); status.Durable || status.Error == "" {
		t.Fatalf("failed write claimed durability: %+v", status)
	}
	store, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := mpc.New(store, nil, "SE3", mpc.Params{})
	if failure := restarted.PlanningFailureSnapshot(); failure == nil || failure.Trigger != "last-durable-attempt" {
		t.Fatalf("a failed write erased the previous durable evidence: %+v", failure)
	}
}
