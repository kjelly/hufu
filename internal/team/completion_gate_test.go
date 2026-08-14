package team

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func acceptedGateInput() CompletionGateInput {
	return CompletionGateInput{
		Result:     &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true},
		Acceptance: &AcceptanceResult{State: AcceptancePassed, Passed: true},
		Evidence: &EvidenceManifest{
			RunID: "run-1", Status: "accepted", ManifestHash: "digest",
			EvidenceResults: []EvidenceResult{{RequirementID: "run:acceptance", Status: "passed"}},
		},
		RequiredTasks: []TaskReference{{ID: "task-1", Status: string(TaskDone)}},
	}
}

func TestApplyCompletionGatePromotesCanonicalSharedCandidates(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "test"}},
		projectDir:     "project",
		executionRunID: "run-shared",
		contextRepo:    repo,
		taskTracker:    NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	c.persistKnowledgeCandidate("use evidence-gated shared memory", ltmSectionPatterns, "ltm_update")
	manifest := &EvidenceManifest{RunID: "run-shared", Status: "accepted", EvidenceResults: []EvidenceResult{{RequirementID: "run:acceptance", Status: "passed"}}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	c.lastEvidenceManifest = manifest
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &AcceptanceResult{State: AcceptancePassed}}
	got := c.applyCompletionGate(context.Background(), result, result.Acceptance)
	if got.Outcome != RunOutcomeCompleted || !got.GoalSatisfied {
		t.Fatalf("completion gate unexpectedly rejected accepted run: %#v", got)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "test"}, Visibility: contextstore.VisibilityExact})
	if err != nil || len(items) != 1 || items[0].Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("shared candidate was not confirmed: %#v err=%v", items, err)
	}
}

func TestCompletionGateRejectsFalseCompletionInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CompletionGateInput)
	}{
		{"worker prose without acceptance", func(in *CompletionGateInput) { in.Acceptance = &AcceptanceResult{State: AcceptanceFailed} }},
		{"missing evidence", func(in *CompletionGateInput) { in.Evidence = nil }},
		{"failed evidence", func(in *CompletionGateInput) { in.Evidence.EvidenceResults[0].Status = "failed" }},
		{"missing artifact digest", func(in *CompletionGateInput) { in.Evidence.ManifestHash = "" }},
		{"unfinished task", func(in *CompletionGateInput) { in.RequiredTasks[0].Status = string(TaskPending) }},
		{"unresolved risk", func(in *CompletionGateInput) { in.UnresolvedRisks = []string{"external state unknown"} }},
		{"terminal leak", func(in *CompletionGateInput) { in.TerminalLeaks = []string{"pty-1"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := acceptedGateInput()
			tc.mutate(&input)
			decision := EvaluateCompletionGate(context.Background(), input)
			if decision.Accepted || len(decision.Reasons) == 0 {
				t.Fatalf("gate decision = %#v, want rejection with reasons", decision)
			}
		})
	}
}

func TestCompletionGateAcceptsOnlyCompleteEvidence(t *testing.T) {
	decision := EvaluateCompletionGate(context.Background(), acceptedGateInput())
	if !decision.Accepted || len(decision.Reasons) != 0 {
		t.Fatalf("gate decision = %#v, want accepted", decision)
	}
}

func TestApplyCompletionGateDowngradesMissingEvidence(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}},
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})
	item := c.taskTracker.TodoList().Items()[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "worker claimed success")
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true}
	result.Acceptance = &AcceptanceResult{State: AcceptancePassed, Passed: true}
	c.lastEvidenceManifest = &EvidenceManifest{RunID: "run", Status: "accepted", ManifestHash: "hash", EvidenceResults: []EvidenceResult{{RequirementID: "run:acceptance", Status: "passed"}}}
	got := c.applyCompletionGate(context.Background(), result, result.Acceptance)
	if got.Outcome != RunOutcomePartial || got.GoalSatisfied || got.StopReason != StopReasonEvidenceIncomplete {
		t.Fatalf("gated result = %#v, want partial evidence_incomplete", got)
	}
}

func TestApplyCompletionGateReadsActualUnresolvedRisk(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}},
		taskTracker: NewTaskTracker(), executionRunID: "run-risk",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.TypedResult = &TaskResult{Risks: []Risk{{Description: "external state is uncertain"}}}
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &AcceptanceResult{State: AcceptancePassed}}
	c.lastEvidenceManifest = &EvidenceManifest{RunID: "run-risk", Status: "accepted", ManifestHash: "hash", EvidenceResults: []EvidenceResult{{RequirementID: "task:1", Status: "passed"}, {RequirementID: "run:acceptance", Status: "passed"}}}
	got := c.applyCompletionGate(context.Background(), result, result.Acceptance)
	if got.Outcome != RunOutcomePartial || got.StopReason != StopReasonEvidenceIncomplete || !strings.Contains(got.Reason, "external state is uncertain") {
		t.Fatalf("risk-gated result = %#v, want actual risk rejection", got)
	}
}

func TestApplyCompletionGateReadsActualTerminalLeak(t *testing.T) {
	ws := t.TempDir()
	manager, err := NewTerminalSessionManager(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-terminal")
	session, err := manager.Start(ctx, TerminalStartRequest{RunID: "run-prior", OwnerTaskID: "task-terminal", Command: []string{"sh", "-c", "sleep 5"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()
	c := &Coordinator{
		session:     &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}},
		taskTracker: NewTaskTracker(), terminalSessionMgr: manager, executionRunID: "run-terminal",
	}
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &AcceptanceResult{State: AcceptancePassed}}
	c.lastEvidenceManifest = &EvidenceManifest{RunID: "run-terminal", Status: "accepted", ManifestHash: "hash", EvidenceResults: []EvidenceResult{{RequirementID: "run:acceptance", Status: "passed"}}}
	got := c.applyCompletionGate(context.Background(), result, result.Acceptance)
	if got.Outcome != RunOutcomePartial || got.StopReason != StopReasonEvidenceIncomplete || !strings.Contains(got.Reason, "leaked terminal sessions") {
		t.Fatalf("terminal-gated result = %#v, want actual leak rejection", got)
	}
}
