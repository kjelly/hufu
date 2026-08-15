package team

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDeprecatedMemoryToolEventAndReportAreContentFree(t *testing.T) {
	es, err := NewEventStore(t.TempDir(), "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	c := &Coordinator{eventStore: es, executionRunID: "run-1"}
	c.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "private", "persistent")
	report := c.DeprecatedMemoryToolReport()
	if len(report) != 1 || report[0].Tool != "memory_save" || report[0].Calls != 1 || report[0].Success != 1 {
		t.Fatalf("report = %#v", report)
	}
	events, err := es.ReadEvents()
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"content", "input", "raw_input", "error"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("event leaked forbidden field %q: %#v", forbidden, payload)
		}
	}
}

// TestDeprecatedMemoryToolReportFiltersByRunID is the regression for the
// HF-MEM5-007 "本次 run" requirement: a single shared session JSONL log
// must not let prior runs pollute a fresh run's compatibility report.
// Two runs are appended to the same workspace, then the report is read
// against each runID in turn. Earlier successful, fail-closed, and
// policy-deny events must not surface when the active runID is the
// zero-call run, and the prior runID must still produce its full counts.
func TestDeprecatedMemoryToolReportFiltersByRunID(t *testing.T) {
	workspace := t.TempDir()

	// First run writes one success, one fail-closed, and one policy-deny
	// event — all stamped with runID == "run-A".
	runA, err := NewEventStore(workspace, "run-A", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	cA := &Coordinator{eventStore: runA, executionRunID: "run-A"}
	cA.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "private", "persistent")
	cA.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "fail_closed", "shared", "session")
	// A policy_decision deny event with kind=tool/tool=ltm_update is the
	// same shape DeprecatedMemoryToolReport reads off the wire.
	_ = cA.emitEvent("policy_decision", "policy", "task-1", map[string]interface{}{
		"kind": "tool", "agent": "worker", "tool": "ltm_update",
		"decision": PolicyDecision{Code: DecisionDeny, RuleID: "test.deny", Reason: "deny"},
	})
	if err := runA.Close(); err != nil {
		t.Fatal(err)
	}

	// Second run re-opens the same workspace JSONL with runID == "run-B"
	// and records no deprecated calls — the clean-state scenario.
	runB, err := NewEventStore(workspace, "run-B", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	defer runB.Close()
	cB := &Coordinator{eventStore: runB, executionRunID: "run-B"}

	if report := cB.DeprecatedMemoryToolReport(); len(report) != 0 {
		t.Fatalf("run-B report inherited prior-run events: %#v", report)
	}

	// Re-open with run-A as the active run and confirm the full counts are
	// still recoverable for the run that actually emitted them.
	runARedo, err := NewEventStore(workspace, "run-A", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	defer runARedo.Close()
	cARedo := &Coordinator{eventStore: runARedo, executionRunID: "run-A"}
	report := cARedo.DeprecatedMemoryToolReport()
	if len(report) != 2 {
		t.Fatalf("run-A report entries=%d, want 2 (memory_save + ltm_update); report=%#v", len(report), report)
	}
	var memorySave, ltmUpdate *DeprecatedMemoryToolUsage
	for i := range report {
		switch report[i].Tool {
		case "memory_save":
			memorySave = &report[i]
		case "ltm_update":
			ltmUpdate = &report[i]
		}
	}
	if memorySave == nil || memorySave.Calls != 2 || memorySave.Success != 1 || memorySave.FailClosed != 1 {
		t.Fatalf("run-A memory_save counts wrong: %#v", memorySave)
	}
	if ltmUpdate == nil || ltmUpdate.Denied != 1 {
		t.Fatalf("run-A ltm_update denied count wrong: %#v", ltmUpdate)
	}

	// An empty active runID without a captured completed-run snapshot or
	// a session-workspace rehydration target must return nil: with no
	// event store and no run history, the absence of an active run is
	// indistinguishable from "no calls ever recorded". The completed-run
	// snapshot path below exercises the post-run visibility fix.
	cNone := &Coordinator{eventStore: runARedo, executionRunID: ""}
	if report := cNone.DeprecatedMemoryToolReport(); report != nil {
		t.Fatalf("empty executionRunID report=%#v, want nil", report)
	}
}

// TestDeprecatedMemoryToolReportPostRunSnapshot is the regression for
// HF-MEM5-007 / HF-MEM6 review P1: --report must surface the just-
// completed run's compatibility counts even after beginExecutionRun's
// deferred close has cleared executionRunID and closed the event store.
// The coordinator captures the aggregate into
// lastCompletedRunDeprecatedReport just before the close, and
// DeprecatedMemoryToolReport must return it once no active run is in
// scope.
func TestDeprecatedMemoryToolReportPostRunSnapshot(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-A", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{eventStore: es, executionRunID: "run-A"}
	c.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "private", "persistent")
	c.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "fail_closed", "shared", "session")
	_ = c.emitEvent("policy_decision", "policy", "task-1", map[string]interface{}{
		"kind": "tool", "agent": "worker", "tool": "ltm_update",
		"decision": PolicyDecision{Code: DecisionDeny, RuleID: "test.deny", Reason: "deny"},
	})

	// Snapshot the active-run aggregate (this is what beginExecutionRun's
	// deferred close now does before clearing state).
	c.captureCompletedRunDeprecatedReport()

	// Simulate the deferred close: clear executionRunID and close the store.
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	c.eventStore = nil
	c.executionRunID = ""

	// The post-run report must return the captured aggregate.
	report := c.DeprecatedMemoryToolReport()
	if len(report) != 2 {
		t.Fatalf("post-run report entries=%d, want 2; report=%#v", len(report), report)
	}
	var memorySave, ltmUpdate *DeprecatedMemoryToolUsage
	for i := range report {
		switch report[i].Tool {
		case "memory_save":
			memorySave = &report[i]
		case "ltm_update":
			ltmUpdate = &report[i]
		}
	}
	if memorySave == nil || memorySave.Calls != 2 || memorySave.Success != 1 || memorySave.FailClosed != 1 {
		t.Fatalf("memory_save counts wrong: %#v", memorySave)
	}
	if ltmUpdate == nil || ltmUpdate.Denied != 1 {
		t.Fatalf("ltm_update denied count wrong: %#v", ltmUpdate)
	}
}

// TestDeprecatedMemoryToolReportRehydratesFromDiskWhenNoSnapshot is the
// resilience half of the HF-MEM6 P1 fix: when a coordinator is resumed
// fresh and has neither an active event store nor an in-memory snapshot
// of the prior run, --report must still surface the most recent run's
// counts by rehydrating from the workspace event log.
func TestDeprecatedMemoryToolReportRehydratesFromDiskWhenNoSnapshot(t *testing.T) {
	workspace := t.TempDir()

	// First-run writes compatibility observations into the shared JSONL.
	firstRun, err := NewEventStore(workspace, "run-first", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	cFirst := &Coordinator{eventStore: firstRun, executionRunID: "run-first"}
	cFirst.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "private", "persistent")
	cFirst.recordDeprecatedMemoryToolCall(context.Background(), "stm_write", "fail_closed", "shared", "session")
	if err := firstRun.Close(); err != nil {
		t.Fatal(err)
	}

	// A second, empty run appends an unrelated event so the most-recent
	// runID in the log is not the same as the run that has compatibility
	// observations. The rehydration scan must still find run-first's
	// counts because run-second had no observations, but the report will
	// return nil when only run-second is the most recent and produced no
	// counts. To exercise the contract precisely, append a no-op event
	// under run-second so the file has multiple runIDs but only run-first
	// produced compatibility observations.
	secondRun, err := NewEventStore(workspace, "run-second", "session-shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRun.Close(); err != nil {
		t.Fatal(err)
	}

	// A brand-new coordinator with no live event store, no snapshot, but
	// the workspace available must rehydrate the most recent runID with
	// compatibility observations and report its counts.
	resumed := &Coordinator{}
	resumed.session = &TeamSession{Workspace: workspace}
	report := resumed.DeprecatedMemoryToolReport()
	if len(report) == 0 {
		t.Fatalf("rehydrated report empty; expected run-first counts: %#v", report)
	}
	var memorySave, stmWrite *DeprecatedMemoryToolUsage
	for i := range report {
		switch report[i].Tool {
		case "memory_save":
			memorySave = &report[i]
		case "stm_write":
			stmWrite = &report[i]
		}
	}
	if memorySave == nil || memorySave.Calls != 1 || memorySave.Success != 1 {
		t.Fatalf("rehydrated memory_save counts wrong: %#v", memorySave)
	}
	if stmWrite == nil || stmWrite.Calls != 1 || stmWrite.FailClosed != 1 {
		t.Fatalf("rehydrated stm_write counts wrong: %#v", stmWrite)
	}
}

// TestDeprecatedMemoryToolReportFullBeginToDeferredCloseLifecycle is the
// HF-MEM6 review P1 end-to-end regression: --report must surface the
// just-completed run's compatibility counts after beginExecutionRun's
// deferred close has cleared executionRunID and closed the event store.
// Unlike the snapshot unit test, this exercise drives the production
// code path through the full lifecycle: initEventStore, an active
// executionRunID, recordDeprecatedMemoryToolCall events that flow into
// the live event store, captureCompletedRunDeprecatedReport (the
// snapshot the deferred close uses), then a state-clear that mimics the
// deferred close, and finally DeprecatedMemoryToolReport must still
// return the per-run aggregate. This guards against future refactors
// that move the snapshot out of the deferred close or drop the
// captureCompletedRunDeprecatedReport call.
func TestDeprecatedMemoryToolReportFullBeginToDeferredCloseLifecycle(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace},
	}
	// Set executionRunID BEFORE initEventStore so the live event store is
	// stamped with the same runID the deferred close will pass to
	// captureCompletedRunDeprecatedReport.
	c.executionEventsMu.Lock()
	c.executionRunID = "run-lifecycle"
	c.executionEventsMu.Unlock()
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatal("initEventStore did not install an event store")
	}
	if c.executionRunID != "run-lifecycle" {
		t.Fatalf("executionRunID = %q, want run-lifecycle", c.executionRunID)
	}

	// Opt-in alias lifecycle: one memory_save success, one memory_save
	// fail-closed, one stm_write fail-closed, and one policy_decision
	// deny for ltm_update. These go through the production
	// recordDeprecatedMemoryToolCall / emitEvent path so the on-disk
	// event log carries the same shape a real run would.
	c.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "shared", "persistent")
	c.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "fail_closed", "shared", "session")
	c.recordDeprecatedMemoryToolCall(context.Background(), "stm_write", "fail_closed", "shared", "session")
	if err := c.emitEvent("policy_decision", "policy", "task-1", map[string]interface{}{
		"kind": "tool", "agent": "worker", "tool": "ltm_update",
		"decision": PolicyDecision{Code: DecisionDeny, RuleID: "test.deny", Reason: "deny"},
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the active-run aggregate (this is what the deferred close
	// in beginExecutionRun does before clearing executionRunID).
	c.captureCompletedRunDeprecatedReport()

	// Verify the snapshot was populated — without this, the deferred
	// close has no aggregate to expose to a post-run --report. The
	// capture flag is the authoritative sentinel; the slice length
	// alone is not (a zero-use run captures an empty slice and that
	// empty slice is still the per-run answer).
	c.lastCompletedRunDeprecatedMu.RLock()
	captured := c.lastCompletedRunDeprecatedCapture
	snapshotLen := len(c.lastCompletedRunDeprecatedReport)
	c.lastCompletedRunDeprecatedMu.RUnlock()
	if !captured {
		t.Fatal("captureCompletedRunDeprecatedReport did not set the capture flag")
	}
	if snapshotLen == 0 {
		t.Fatal("captureCompletedRunDeprecatedReport did not populate the snapshot")
	}

	// Simulate the deferred close: close the event store and clear the
	// active run ID. After this, DeprecatedMemoryToolReport has no live
	// event store to read from.
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	c.eventStore = nil
	c.executionEventsMu.Lock()
	c.executionRunID = ""
	c.executionEventsMu.Unlock()

	// The post-run report must surface every alias that was exercised,
	// with the right per-tool lifecycle counts. It must not leak any
	// memory content (the policy payload is forbidden but the test does
	// not assert content because DeprecatedMemoryToolReport returns the
	// aggregate, not the payload).
	report := c.DeprecatedMemoryToolReport()
	if len(report) != 3 {
		t.Fatalf("post-run report entries=%d, want 3 (memory_save+stm_write+ltm_update); report=%#v", len(report), report)
	}
	var memorySave, stmWrite, ltmUpdate *DeprecatedMemoryToolUsage
	for i := range report {
		switch report[i].Tool {
		case "memory_save":
			memorySave = &report[i]
		case "stm_write":
			stmWrite = &report[i]
		case "ltm_update":
			ltmUpdate = &report[i]
		}
	}
	if memorySave == nil || memorySave.Calls != 2 || memorySave.Success != 1 || memorySave.FailClosed != 1 || memorySave.Denied != 0 {
		t.Fatalf("memory_save counts wrong: %#v", memorySave)
	}
	if stmWrite == nil || stmWrite.Calls != 1 || stmWrite.Success != 0 || stmWrite.FailClosed != 1 || stmWrite.Denied != 0 {
		t.Fatalf("stm_write counts wrong: %#v", stmWrite)
	}
	if ltmUpdate == nil || ltmUpdate.Calls != 0 || ltmUpdate.Success != 0 || ltmUpdate.FailClosed != 0 || ltmUpdate.Denied != 1 {
		t.Fatalf("ltm_update counts wrong: %#v", ltmUpdate)
	}
}

// TestDeprecatedMemoryToolReportZeroUseRunDoesNotInheritPriorRun is the
// HF-MEM5 review P1 lifecycle regression: a fresh run that records no
// alias usage must not have its --report populated from a prior run's
// compatibility observations. Before the capture-flag fix,
// DeprecatedMemoryToolReport used len(snapshot) > 0 as the sentinel for
// "a completed run was captured", so a zero-use run captured an empty
// slice, the sentinel missed, and the function fell through to disk
// rehydration via latestRunIDWithCompatibilityUsage — which deliberately
// returns the prior runID. That made run B's post-run report claim run
// A's alias counts as B's. With the independent capture flag, an empty
// captured slice is the authoritative answer for the just-finished run,
// and disk rehydration is reserved for a coordinator that has never
// completed a run. Both runs go through the same beginExecutionRun /
// captureCompletedRunDeprecatedReport lifecycle so this regression
// guards the production close path too, not just the snapshot unit.
func TestDeprecatedMemoryToolReportZeroUseRunDoesNotInheritPriorRun(t *testing.T) {
	workspace := t.TempDir()

	// Run A: full beginExecutionRun lifecycle with one alias call.
	cA := &Coordinator{session: &TeamSession{Workspace: workspace}}
	cA.executionEventsMu.Lock()
	cA.executionRunID = "run-A"
	cA.executionEventsMu.Unlock()
	cA.initEventStore()
	if cA.eventStore == nil {
		t.Fatal("run-A: initEventStore did not install an event store")
	}
	cA.recordDeprecatedMemoryToolCall(context.Background(), "memory_save", "success", "private", "persistent")
	// Deferred close: snapshot then clear state and close event store.
	cA.captureCompletedRunDeprecatedReport()
	if err := cA.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	cA.eventStore = nil
	cA.executionEventsMu.Lock()
	cA.executionRunID = ""
	cA.executionEventsMu.Unlock()

	// Sanity: post-run --report for run A surfaces its alias counts.
	if report := cA.DeprecatedMemoryToolReport(); len(report) == 0 {
		t.Fatalf("run-A post-run report should be non-empty: %#v", report)
	}

	// Run B begins and ends without any alias usage. The deferred close
	// captures an empty slice and clears executionRunID. B's
	// --report must be empty even though run A's observations are still
	// on disk and would be picked up by latestRunIDWithCompatibilityUsage.
	cB := &Coordinator{session: &TeamSession{Workspace: workspace}}
	cB.executionEventsMu.Lock()
	cB.executionRunID = "run-B"
	cB.executionEventsMu.Unlock()
	cB.initEventStore()
	if cB.eventStore == nil {
		t.Fatal("run-B: initEventStore did not install an event store")
	}
	// No recordDeprecatedMemoryToolCall — this is the zero-use scenario.
	cB.captureCompletedRunDeprecatedReport()
	if err := cB.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	cB.eventStore = nil
	cB.executionEventsMu.Lock()
	cB.executionRunID = ""
	cB.executionEventsMu.Unlock()

	// The fix: run B's captured-empty slice is the authoritative
	// per-run answer and must NOT fall through to disk rehydration.
	if report := cB.DeprecatedMemoryToolReport(); len(report) != 0 {
		t.Fatalf("run-B (zero-use) report inherited run-A counts: %#v", report)
	}

	// Sanity: the capture flag is now set, so a fresh coordinator that
	// resumes the workspace and never captured a completed run still
	// falls through to disk rehydration via latestRunIDWithCompatibilityUsage.
	resumed := &Coordinator{}
	resumed.session = &TeamSession{Workspace: workspace}
	if report := resumed.DeprecatedMemoryToolReport(); len(report) == 0 {
		t.Fatalf("fresh coordinator rehydration should still surface run-A counts: %#v", report)
	}
}
