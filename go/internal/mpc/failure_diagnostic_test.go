package mpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type diagnosticFailureTransport struct {
	request, response []byte
	err               error
}

func TestFailureEvidenceKeepsRejectedDownsideInputs(t *testing.T) {
	svc := shadowTestService(t)
	start := time.Now().UTC().Truncate(time.Hour)
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	svc.PV = func(time.Time, float64) float64 { return 1000 }
	svc.PVForecastSafetyK = 1
	svc.PVUncertaintyW = func() float64 { return math.NaN() }
	plan := svc.Replan(context.Background())
	if plan == nil || len(plan.Actions) != 4 {
		t.Fatal("invalid optional downside forecast blocked a valid point plan")
	}
	failure := svc.PlanningFailureSnapshot()
	if failure == nil || failure.Stage != "risk_inputs" {
		t.Fatalf("rejected downside evidence was overwritten: %+v", failure)
	}
	var inputs struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(failure.Inputs, &inputs); err != nil || inputs.Params["PVUncertaintyW"] != "NaN" {
		t.Fatalf("failure lost the raw uncertainty before caution was disabled: %s %v", failure.Inputs, err)
	}
	restored := New(svc.Store, nil, svc.Zone, svc.Defaults).PlanningFailureSnapshot()
	if restored == nil || !bytes.Equal(restored.Inputs, failure.Inputs) || restored.Stage != "risk_inputs" {
		t.Fatal("rejected risk inputs were not durable")
	}
}

func (f *diagnosticFailureTransport) RoundTrip(_ context.Context, payload []byte) ([]byte, error) {
	f.request = append([]byte(nil), payload...)
	if f.err != nil {
		return f.response, f.err
	}
	var request externalRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	f.response, _ = json.Marshal(map[string]any{"schema_version": 1, "request_id": request.RequestID,
		"ok": false, "error": map[string]string{"code": "infeasible", "message": "test forecast exceeds site limits"}})
	return f.response, nil
}
func (f *diagnosticFailureTransport) Close() error { return nil }

func TestFailureEvidenceKeepsExactWorkerExchangeAfterRecovery(t *testing.T) {
	svc := shadowTestService(t)
	transport := &diagnosticFailureTransport{}
	svc.Optimizer = &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: transport}
	plan := svc.Replan(context.Background())
	if plan == nil || svc.PlanSnapshot().Outdated {
		t.Fatal("fixture did not recover")
	}
	failure := svc.PlanningFailureSnapshot()
	if failure == nil || failure.Stage != "primary_optimizer" || failure.AttemptStartedAtMs <= 0 || failure.CapturedAtMs < failure.AttemptStartedAtMs {
		t.Fatalf("missing attempt identity or stage: %+v", failure)
	}
	if failure.OptimizerRequest == nil || !bytes.Equal(failure.OptimizerRequest.JSON, transport.request) ||
		failure.OptimizerResponse == nil || !bytes.Equal(failure.OptimizerResponse.JSON, transport.response) {
		t.Fatalf("worker evidence was reconstructed or lost: %+v", failure)
	}
	if failure.RecoveredDecisionID != plan.DecisionID || failure.RecoveredAtMs <= 0 || svc.Diagnose().DecisionID != plan.DecisionID {
		t.Fatalf("failure and recovery were confused: %+v", failure)
	}
	// Neither a support reader nor a reused transport buffer owns the retained evidence.
	failure.Inputs[0], failure.OptimizerRequest.JSON[0], transport.response[0] = '!', '!', '!'
	retained := svc.PlanningFailureSnapshot()
	if !json.Valid(retained.Inputs) || !json.Valid(retained.OptimizerRequest.JSON) || !json.Valid(retained.OptimizerResponse.JSON) {
		t.Fatal("retained evidence shares mutable buffers")
	}
}

func TestFailureEvidenceRetainsEffectiveLimitsWhenFallbackFails(t *testing.T) {
	svc := shadowTestService(t)
	start := time.Now().UTC().Truncate(time.Hour)
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	svc.FuseMaxW, svc.BaseLoad = 1000, 40000
	transport := &diagnosticFailureTransport{}
	svc.Optimizer = &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: transport}
	if svc.Replan(context.Background()) != nil {
		t.Fatal("impossible current slot became a plan")
	}
	failure := svc.PlanningFailureSnapshot()
	if failure == nil || failure.Stage != "core_fallback" || failure.RecoveredDecisionID != "" {
		t.Fatalf("lost total failure: %+v", failure)
	}
	var inputs struct {
		Slots []Slot `json:"slots"`
	}
	if err := json.Unmarshal(failure.Inputs, &inputs); err != nil || len(inputs.Slots) != 4 || inputs.Slots[0].Limits.MaxImportW != 1000 || inputs.Slots[0].LoadW != 40000 {
		t.Fatalf("captured placeholders instead of failed solver inputs: %+v %v", inputs, err)
	}
	if failure.OptimizerRequest == nil || !bytes.Equal(failure.OptimizerRequest.JSON, transport.request) {
		t.Fatal("the primary failure disappeared when its fallback failed")
	}
}

func TestFailureEvidencePreservesInvalidNumbersAndExcludesRuntimeProof(t *testing.T) {
	slots, p := recoveryFixture()
	p.ChargeEfficiency, p.MaxChargeW, slots[0].PVW = math.NaN(), math.Inf(1), math.Inf(-1)
	p.PVCurtailment.Proof = "runtime-proof-must-not-leave"
	raw, truncated := encodeFailureInputs(map[string]any{"params": p, "slots": slots})
	if truncated || !json.Valid(raw) || bytes.Contains(raw, []byte(p.PVCurtailment.Proof)) {
		t.Fatalf("invalid numbers broke evidence or runtime proof leaked: %s", raw)
	}
	for _, want := range []string{`"ChargeEfficiency":"NaN"`, `"MaxChargeW":"+Inf"`, `"PVW":"-Inf"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("missing invalid value %s", want)
		}
	}
}

func TestFailureEvidenceBoundsMalformedRepliesAndRetainedInputs(t *testing.T) {
	slots, p := recoveryFixture()
	transport := &diagnosticFailureTransport{response: []byte("partial response"), err: context.DeadlineExceeded}
	optimizer := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: transport}
	_, err := optimizer.Optimize(context.Background(), slots, p)
	var wire *optimizerFailureError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &wire) || wire.response.Text != "partial response" {
		t.Fatalf("timeout cause or partial reply lost: %v", err)
	}
	large := captureFailurePayload(bytes.Repeat([]byte("x"), 2*planningFailureByteLimit))
	if !large.Truncated || large.SizeBytes != 2*planningFailureByteLimit || large.Text != "" || large.JSON != nil {
		t.Fatal("oversized reply was presented as complete JSON")
	}
	svc := shadowTestService(t)
	r := svc.beginReplan(context.Background(), "test-limit")
	defer svc.finishReplan(r)
	for len(slots) <= 600 {
		slots = append(slots, slots[0])
	}
	svc.recordPlanningFailure(r, "inputs", "too many rows", slots, p, nil)
	failure := svc.PlanningFailureSnapshot()
	if failure == nil || !failure.InputsTruncated || len(failure.Inputs) > planningFailureByteLimit {
		t.Fatal("retained failure is unbounded or silently truncated")
	}
	var inputs struct {
		Slots     []Slot `json:"slots"`
		Requested int    `json:"requested_slots"`
	}
	if err := json.Unmarshal(failure.Inputs, &inputs); err != nil || inputs.Requested != len(slots) || len(inputs.Slots) != 512 {
		t.Fatalf("capture bound lost original row count: %+v %v", inputs, err)
	}
	longError := `{"access_token":"` + strings.Repeat("secret", 2000) + `"}`
	svc.recordPlanningFailure(r, "inputs", longError, slots[:1], p, nil)
	failure = svc.PlanningFailureSnapshot()
	if !failure.ErrorTruncated || strings.Contains(failure.Error, "secret") {
		t.Fatal("an oversized error retained a credential fragment")
	}
}

type supersededFailureOptimizer struct {
	calls            atomic.Int32
	started, release chan struct{}
}

func (o *supersededFailureOptimizer) Optimize(context.Context, []Slot, Params) (Plan, error) {
	if o.calls.Add(1) == 1 {
		close(o.started)
		<-o.release
		return Plan{}, errors.New("old superseded failure")
	}
	return Plan{}, errors.New("new current failure")
}
func (o *supersededFailureOptimizer) Close() error { return nil }

func TestFailureEvidenceCannotBeOverwrittenBySupersededWork(t *testing.T) {
	svc := shadowTestService(t)
	o := &supersededFailureOptimizer{started: make(chan struct{}), release: make(chan struct{})}
	svc.Optimizer = o
	done := make(chan struct{})
	go func() { defer close(done); svc.Replan(context.Background()) }()
	select {
	case <-o.started:
	case <-time.After(2 * time.Second):
		t.Fatal("old solve did not start")
	}
	newPlan := svc.Replan(context.Background())
	close(o.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded solve did not finish")
	}
	failure := svc.PlanningFailureSnapshot()
	if newPlan == nil || failure == nil || failure.Error != "new current failure" || failure.RecoveredDecisionID != newPlan.DecisionID {
		t.Fatalf("stale attempt replaced current evidence: %+v", failure)
	}
	restored := New(svc.Store, nil, svc.Zone, svc.Defaults).PlanningFailureSnapshot()
	if restored == nil || restored.Error != failure.Error || restored.RecoveredDecisionID != newPlan.DecisionID {
		t.Fatalf("stale attempt replaced durable evidence: %+v", restored)
	}
}

func TestFailureEvidenceStorageRestoresWireAndRecoveryWithoutExecution(t *testing.T) {
	svc := shadowTestService(t)
	transport := &diagnosticFailureTransport{}
	svc.Optimizer = &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: transport}
	plan := svc.Replan(context.Background())
	if plan == nil {
		t.Fatal("fixture did not recover")
	}
	restarted := New(svc.Store, nil, svc.Zone, svc.Defaults)
	failure := restarted.PlanningFailureSnapshot()
	if failure == nil || failure.RecoveredDecisionID != plan.DecisionID ||
		!bytes.Equal(failure.OptimizerRequest.JSON, transport.request) || !bytes.Equal(failure.OptimizerResponse.JSON, transport.response) {
		t.Fatalf("durable worker exchange or recovery was lost: %+v", failure)
	}
	status := restarted.PlanningFailureStorageSnapshot()
	if !status.Enabled || !status.Durable || !status.Restored || status.Error != "" {
		t.Fatalf("restore status is wrong: %+v", status)
	}
	if restarted.Latest() != nil || restarted.Diagnose() != nil {
		t.Fatal("restoring failure evidence restored a plan")
	}
	if _, ok := restarted.SlotDirectiveAt(time.Now()); ok {
		t.Fatal("restoring evidence authorized dispatch")
	}
	// A later success marks the old failure as recovered, without erasing it.
	restarted.BaseLoad = 500
	if restarted.Replan(context.Background()) == nil || restarted.PlanningFailureSnapshot().RecoveredDecisionID != plan.DecisionID {
		t.Fatal("a clean replan erased the failure or rewrote its first recovery")
	}
	// Unchanged evidence requires no disk write on a later successful replan.
	_ = svc.Store.Close()
	restarted.persistPlanningFailure()
	if status := restarted.PlanningFailureStorageSnapshot(); !status.Durable || status.Error != "" {
		t.Fatalf("unchanged evidence was written again: %+v", status)
	}
}

func TestFailureEvidenceStorageRejectsCorruptRecordsAndReportsReadErrors(t *testing.T) {
	svc := shadowTestService(t)
	for _, raw := range []string{`{`, `null`, `{"schema_version":2}`, `{"schema_version":1,"inputs":{}}`, strings.Repeat(" ", planningFailureRecordByteLimit+1)} {
		if err := svc.Store.SaveConfig(planningFailureConfigKey, raw); err != nil {
			t.Fatal(err)
		}
		restarted := New(svc.Store, nil, svc.Zone, svc.Defaults)
		if restarted.PlanningFailureSnapshot() != nil || restarted.Latest() != nil || restarted.PlanningFailureStorageSnapshot().Error == "" {
			t.Fatal("corrupt evidence was accepted or its restore error was hidden")
		}
	}
	_ = svc.Store.Close()
	restarted := New(svc.Store, nil, svc.Zone, svc.Defaults)
	if restarted.PlanningFailureSnapshot() != nil || restarted.PlanningFailureStorageSnapshot().Error == "" {
		t.Fatal("storage read failure was hidden")
	}
}

func TestFailureEvidenceStorageFailureKeepsMemoryAndReportsError(t *testing.T) {
	svc := shadowTestService(t)
	_ = svc.Store.Close()
	svc.Replan(context.Background())
	failure := svc.PlanningFailureSnapshot()
	status := svc.PlanningFailureStorageSnapshot()
	if failure == nil || status.Durable || status.Error == "" || !strings.Contains(failure.Error, "Could not load electricity prices") {
		t.Fatalf("failed write lost evidence or claimed durability: failure=%+v status=%+v", failure, status)
	}
}

func TestFailureEvidenceStorageMarksRecoveryAfterRestart(t *testing.T) {
	svc := shadowTestService(t)
	valid := svc.Defaults
	svc.Defaults.ChargeEfficiency = math.NaN()
	svc.Replan(context.Background())
	failure := svc.PlanningFailureSnapshot()
	if failure == nil || failure.RecoveredDecisionID != "" {
		t.Fatal("fixture did not retain an unrecovered failure")
	}
	restarted := New(svc.Store, nil, svc.Zone, valid)
	restarted.BaseLoad = 500
	plan := restarted.Replan(context.Background())
	if plan == nil {
		t.Fatal("valid restart did not recover")
	}
	retained := New(svc.Store, nil, svc.Zone, valid).PlanningFailureSnapshot()
	if retained == nil || retained.CapturedAtMs != failure.CapturedAtMs || retained.RecoveredDecisionID != plan.DecisionID || retained.RecoveredAtMs <= 0 {
		t.Fatalf("recovery after restart lost the original evidence or was not durable: %+v", retained)
	}
}

func TestFailureEvidenceStorageDoesNotHoldDispatchLock(t *testing.T) {
	svc := shadowTestService(t)
	start := time.Now().UTC().Truncate(time.Hour)
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	svc.Optimizer = &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: &diagnosticFailureTransport{}}
	svc.failurePersistMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			svc.failurePersistMu.Unlock()
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); svc.Replan(context.Background()) }()
	waitFor(t, "accepted recovery before diagnostic write", func() bool { return svc.Latest() != nil })
	if _, ok := svc.SlotDirectiveAt(svc.planningNow()); !ok {
		t.Fatal("pending diagnostic I/O held up an accepted directive")
	}
	if status := svc.PlanningFailureStorageSnapshot(); status.Durable {
		t.Fatal("pending I/O was reported as durable")
	}
	svc.failurePersistMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic write did not finish")
	}
	if status := svc.PlanningFailureStorageSnapshot(); !status.Durable || status.Error != "" {
		t.Fatalf("finished write is not durable: %+v", status)
	}
}

func TestNativeSupportFailureEvidenceMatchesRejectedWorkerRequest(t *testing.T) {
	worker := nativeWorker(t, time.Second)
	defer worker.Close()
	svc := shadowTestService(t)
	svc.FuseMaxW = 11040
	start := time.Now().UTC().Truncate(time.Hour)
	svc.now = func() time.Time { return start.Add(5 * time.Minute) }
	svc.Load = func(at time.Time) float64 {
		if !at.Before(start.Add(2 * time.Hour)) {
			return 40000
		}
		return 500
	}
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
	plan := svc.Replan(context.Background())
	failure := svc.PlanningFailureSnapshot()
	if plan == nil || len(plan.Actions) != 4 || failure == nil || failure.OptimizerRequest == nil || failure.OptimizerResponse == nil {
		t.Fatalf("native failure or safe recovery missing: plan=%v failure=%+v", plan != nil, failure)
	}
	var request externalRequest
	var response externalResponse
	if json.Unmarshal(failure.OptimizerRequest.JSON, &request) != nil || json.Unmarshal(failure.OptimizerResponse.JSON, &response) != nil ||
		request.RequestID != response.RequestID || response.OK || len(request.Slots) != 4 || request.Slots[2].LoadW != 40000 || !strings.Contains(failure.Error, "infeasible") {
		t.Fatal("captured exchange does not reproduce the rejected native request")
	}
	if plan.Solver.Engine != "native" || plan.Solver.Fallback || failure.RecoveredDecisionID != plan.DecisionID {
		t.Fatal("simpler forecast did not recover native price planning")
	}
	if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
		t.Fatal(err)
	}
}
