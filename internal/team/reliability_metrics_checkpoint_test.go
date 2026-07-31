package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestAcceptanceRevisionPersistsToSessionAndEventStore(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "revision"}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	c.SetAcceptanceSpec(AcceptanceSpec{Commands: []string{"test -f initial"}})
	end := c.beginExecutionRun()
	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: []string{"test -f updated"}}, "review-fix")
	end()

	saved := LoadSession(workspace)
	if saved == nil || len(saved.AcceptanceContractRevisions) != 2 {
		t.Fatalf("acceptance revisions not persisted: %#v", saved)
	}
	if got := saved.AcceptanceContractRevisions[1].Reason; got != "review-fix" {
		t.Fatalf("revision reason = %q", got)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "acceptance_contract_modified" {
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) == nil && payload["reason"] == "review-fix" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("acceptance revision event missing from event store")
	}

	restored := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "revision"}}, taskTracker: NewTaskTracker()}
	restored.SetSessionData(saved)
	if got := restored.acceptanceSpec.Commands[0]; got != "test -f updated" {
		t.Fatalf("restored acceptance command = %q", got)
	}
}

func TestContinuationCheckpointSurvivesRestartAndAbort(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "checkpoint"}}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	c.saveContinuationCheckpoint(2, 5, "step_limit", "pending")
	restarted := &Coordinator{session: c.session, taskTracker: NewTaskTracker()}
	restarted.SetSessionData(LoadSession(workspace))
	if cp := restarted.ContinuationCheckpoint(); cp == nil || cp.Status != "pending" {
		t.Fatalf("pending checkpoint not restored: %#v", cp)
	}
	if cp := restarted.ResumeContinuationCheckpoint(); cp == nil || cp.Status != "resumed" {
		t.Fatalf("checkpoint not resumed: %#v", cp)
	}
	if restarted.continuationResume == nil || restarted.continuationResume.TurnCount != 2 {
		t.Fatalf("resume context missing original turn: %#v", restarted.continuationResume)
	}
	restarted.saveContinuationCheckpoint(3, 5, "provider", "pending")
	restarted.recordRunAborted(context.Canceled)
	restarted2 := &Coordinator{session: c.session, taskTracker: NewTaskTracker()}
	restarted2.SetSessionData(LoadSession(workspace))
	if cp := restarted2.ContinuationCheckpoint(); cp == nil || cp.Status != "aborted" || cp.TurnCount != 3 {
		t.Fatalf("aborted checkpoint not restored: %#v", cp)
	}
}

func TestCoordinatorMetricsAreExternallyQueryableAndCopied(t *testing.T) {
	c := &Coordinator{}
	c.recordRetry(FailureProtocol)
	c.recordRetry(FailureProtocol)
	c.recordRetry(FailureTimeout)
	c.recordCompaction()
	m := c.Metrics()
	if m.RetriesByFailureClass[FailureProtocol] != 2 || m.RetriesByFailureClass[FailureTimeout] != 1 || m.Compactions != 1 {
		t.Fatalf("unexpected metrics: %#v", m)
	}
	m.RetriesByFailureClass[FailureProtocol] = 99
	if got := c.Metrics().RetriesByFailureClass[FailureProtocol]; got != 2 {
		t.Fatalf("metrics map was not copied: %d", got)
	}
}

func TestAcceptanceSpecDeepCopyPreventsCallerMutation(t *testing.T) {
	workspace := t.TempDir()
	commands := []string{"test -f required-artifact"}
	artifacts := []string{"required-artifact"}
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "immutability"}}, sessionData: NewSession(), reportStatus: func(StatusEvent) {}}
	c.SetAcceptanceSpec(AcceptanceSpec{Commands: commands, RequiredArtifacts: artifacts})

	// Mutating the caller-owned backing arrays must not weaken the fixed
	// contract or rewrite its persisted revision snapshot.
	commands[0] = "true"
	artifacts[0] = "other-artifact"
	if got := c.acceptanceSpec.Commands[0]; got != "test -f required-artifact" {
		t.Fatalf("acceptance command changed through caller alias: %q", got)
	}
	if got := c.acceptanceSpec.RequiredArtifacts[0]; got != "required-artifact" {
		t.Fatalf("required artifact changed through caller alias: %q", got)
	}
	first := c.sessionData.AcceptanceContractRevisions[0]
	if first.NewSpec.Commands[0] != "test -f required-artifact" || first.NewSpec.RequiredArtifacts[0] != "required-artifact" {
		t.Fatalf("revision snapshot was mutated through caller alias: %#v", first)
	}

	nextCommands := []string{"test -f updated-artifact"}
	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: nextCommands}, "immutable-test")
	nextCommands[0] = "true"
	if got := c.sessionData.AcceptanceContractRevisions[1].NewSpec.Commands[0]; got != "test -f updated-artifact" {
		t.Fatalf("new revision changed through caller alias: %q", got)
	}

	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: []string{"true"}}, "result-copy-test")
	result, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("runAcceptance failed: %v", err)
	}
	result.Commands[0] = "false"
	if got := c.acceptanceSpec.Commands[0]; got != "true" {
		t.Fatalf("acceptance result exposed internal command slice: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, sessionFile)); err != nil {
		t.Fatalf("expected isolated session artifact in temp workspace: %v", err)
	}
}

func TestCriterionCheckpointPersistsAndRejectsStaleAcceptanceContract(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "ready"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "checkpoint"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		projectDir:   workspace,
		reportStatus: func(StatusEvent) {},
	}
	c.SetAcceptanceSpec(AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "ready", Required: true, Verify: VerificationSpec{Type: VerifyFileExists, Path: "ready"}}}})
	item := &TodoItem{ID: "external-1", Status: TaskDone, Advances: []string{"ready"}, Recovery: RecoveryReconcile, Execution: ExecutionContract{Kind: ExecutionKindExternal}}
	results, err := c.evaluateCriteria(context.Background(), c.acceptanceSpec.Criteria)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = TaskInProgress
	c.recordCriterionCheckpoints(item, results)
	if len(c.sessionData.CriterionCheckpoints) != 0 {
		t.Fatalf("in-progress task must not create a criterion checkpoint: %#v", c.sessionData.CriterionCheckpoints)
	}
	item.Status = TaskDone
	c.recordCriterionCheckpoints(item, results)
	if len(c.sessionData.CriterionCheckpoints) != 1 || !c.sessionData.CriterionCheckpoints[0].Proven {
		t.Fatalf("missing proven checkpoint: %#v", c.sessionData.CriterionCheckpoints)
	}
	if err := c.validateCriterionCheckpoint(item); err != nil {
		t.Fatalf("fresh checkpoint rejected: %v", err)
	}
	if saved := LoadSession(workspace); saved == nil || len(saved.CriterionCheckpoints) != 1 {
		t.Fatalf("checkpoint did not survive session save: %#v", saved)
	}

	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "ready", Required: true, Verify: VerificationSpec{Type: VerifyFileAbsent, Path: "ready"}}}}, "contract changed")
	if err := c.validateCriterionCheckpoint(item); err == nil {
		t.Fatal("stale checkpoint unexpectedly accepted after contract revision")
	}
}

func TestMetricsIncludeOutcomeReliabilitySignals(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "external", Advances: []string{"ready"}}})[0]
	item.Status = TaskError
	item.VerifyResult = &VerificationResult{ExitCode: 1, WeakWarning: true}
	item.ExecutionReceipts = []ExecutionReceipt{{RepairProvenance: &RepairProvenance{Attempted: true, Success: true}}}
	c := &Coordinator{taskTracker: tracker, sessionData: &SessionData{CriterionResults: []CriterionResult{{ID: "ready", State: CriterionPassed}}}}
	c.recordReliabilityUsage("task", 1, 42)
	m := c.Metrics()
	if m.AcceptanceCriteriaPassed != 1 || m.TasksByCriterion["ready"] != 1 || m.ProtocolRepairsAttempted != 1 || m.ProtocolRepairsSucceeded != 1 || m.WorkerSuccessRejected != 1 || m.WeakVerifierWarnings != 1 || m.TokensSinceCriterionProgress != 42 {
		t.Fatalf("unexpected reliability metrics: %#v", m)
	}
}

func TestMetricsTrackProgressTimestampAndPlanUsageWithoutDoubleCounting(t *testing.T) {
	c := &Coordinator{sessionData: &SessionData{
		LastCriterionProgressAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339Nano),
		CriterionResults:        []CriterionResult{{ID: "ready", State: CriterionPassed, EvaluatedAt: time.Now()}},
	}}
	// A plan submission consumes tokens. The final event for the same attempt
	// is cumulative, so only the incremental five tokens may be added.
	c.recordExecutionEvent("task", "worker", 1, "planned", "model", 0, ExecutionUsage{TotalTokens: 10})
	c.recordExecutionEvent("task", "worker", 1, "done", "model", 0, ExecutionUsage{TotalTokens: 15})
	m := c.Metrics()
	if m.TokensSinceCriterionProgress != 15 {
		t.Fatalf("tokens since progress = %d, want 15", m.TokensSinceCriterionProgress)
	}
	if m.TimeSinceCriterionProgressSeconds < 60 {
		t.Fatalf("time since progress = %ds, expected original progress timestamp to survive no-progress evaluation", m.TimeSinceCriterionProgressSeconds)
	}
}

func TestResumeReconcileRevalidatesCurrentCriterionBeforeMarkingDone(t *testing.T) {
	workspace := t.TempDir()
	makeCoordinator := func() (*Coordinator, *TodoItem) {
		tracker := NewTaskTracker()
		item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "external", Verify: "test -f operation-complete", Recovery: RecoveryReconcile, Advances: []string{"ready"}, Execution: ExecutionContract{Kind: ExecutionKindExternal}}})[0]
		tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "interrupted")
		c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "resume"}}, sessionData: NewSession(), taskTracker: tracker, projectDir: workspace, reportStatus: func(StatusEvent) {}}
		c.SetAcceptanceSpec(AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "ready", Required: true, Verify: VerificationSpec{Type: VerifyFileExists, Path: "ready"}}}})
		return c, item
	}

	// A reconcile probe can say the operation completed while the acceptance
	// criterion has changed externally. The task must remain blocked.
	if err := os.WriteFile(filepath.Join(workspace, "operation-complete"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, item := makeCoordinator()
	// This was the old, previously-proven checkpoint. Recovery must not trust
	// it after reconciliation; the live criterion is re-evaluated below.
	c.sessionData.CriterionCheckpoints = []CriterionCheckpoint{{ID: "old", TaskID: item.ID, CriterionID: "ready", Proven: true, InputFingerprint: "old", Evidence: []*VerificationResult{{Fingerprint: "old"}}}}
	if _, err := c.ResumeInterruptedTasks(context.Background()); err != nil {
		t.Fatalf("resume returned unexpected error: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskBlocked {
		t.Fatalf("stale external state marked %s, want blocked (item=%#v)", got, item)
	}

	if err := os.WriteFile(filepath.Join(workspace, "ready"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ = makeCoordinator()
	endRun := c.beginExecutionRun()
	c.antiThrashing = AntiThrashingState{
		RepairsByCriterion:  map[string]int{"ready": 2},
		BlockedCriteria:     map[string]bool{"ready": true},
		BlockedScopes:       map[string]bool{antiThrashingScopeKey("ready", TaskKindRepair): true},
		BlockedFingerprints: map[string]bool{},
	}
	if _, err := c.ResumeInterruptedTasks(context.Background()); err != nil {
		t.Fatalf("fresh resume returned unexpected error: %v", err)
	}
	endRun()
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("fresh reconciliation status = %s, want done", got)
	}
	if len(c.sessionData.CriterionCheckpoints) != 1 || !c.sessionData.CriterionCheckpoints[0].Proven {
		t.Fatalf("fresh reconciliation did not save proven checkpoint: %#v", c.sessionData.CriterionCheckpoints)
	}
	if got := c.Metrics().RepairAttemptsByCriterion["ready"]; got != 0 || c.antiThrashingHardBlocked() {
		t.Fatalf("reconciliation progress did not clear anti-thrashing scope: metrics=%#v state=%#v", c.Metrics(), c.antiThrashing)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if len(replayed.CriterionResults) != 1 || replayed.CriterionResults[0].State != CriterionPassed || replayed.LastCriterionProgressAt == "" {
		t.Fatalf("recovery progress was not replayable: %#v", replayed)
	}
	st := NewSessionTree()
	if err := RebuildSessionForBranch(workspace, st, es, "main"); err != nil {
		t.Fatal(err)
	}
	_ = es.Close()
	checkedOut := LoadSession(workspace)
	if checkedOut == nil || checkedOut.LastCriterionProgressAt == "" || len(checkedOut.CriterionCheckpoints) != 1 || len(checkedOut.CriterionResults) != 1 || checkedOut.CriterionResults[0].State != CriterionPassed {
		t.Fatalf("checkout lost recovered criterion progress: %#v", checkedOut)
	}
}

func TestCriterionCheckpointSurvivesEventReplay(t *testing.T) {
	checkpoint := CriterionCheckpoint{ID: "cp-1", TaskID: "task-1", CriterionID: "ready", Proven: true, InputFingerprint: "fingerprint", CreatedAt: "2026-07-30T00:00:00Z"}
	payload, err := json.Marshal(map[string]any{"checkpoint": checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData([]RunEvent{{Type: "criterion_checkpoint_saved", TaskID: "task-1", Payload: payload}})
	if len(replayed.CriterionCheckpoints) != 1 || replayed.CriterionCheckpoints[0].ID != checkpoint.ID || !replayed.CriterionCheckpoints[0].Proven {
		t.Fatalf("checkpoint replay mismatch: %#v", replayed.CriterionCheckpoints)
	}
}
