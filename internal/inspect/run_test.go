package inspect

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectRunUsesCanonicalRunFinished(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectRun(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := envelope.Data.(RunData)
	if !ok {
		t.Fatalf("run data type = %T", envelope.Data)
	}
	if data.RunID != fixture.runID || data.Outcome != string(team.RunOutcomePartial) || data.TerminalEventID == "" {
		t.Fatalf("run data = %#v", data)
	}
	if data.TaskSummary.Total != 1 || data.TaskSummary.Done != 1 || data.AttemptSummary.Total != 1 {
		t.Fatalf("run summaries = tasks %#v attempts %#v", data.TaskSummary, data.AttemptSummary)
	}
	if envelope.Query.Workspace != "" || envelope.Query.BranchID != "main" {
		t.Fatalf("safe resolved query = %#v", envelope.Query)
	}
}

func TestInspectTaskUsesFrozenExecutionTargetAndHidesRawEvidence(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectTask(t.Context(), InspectQuery{
		Workspace: fixture.workspace,
		RunID:     fixture.runID,
		TaskID:    fixture.taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(TaskData)
	if data.ExecutionTarget != "ollama/frozen-model" {
		t.Fatalf("execution target = %q", data.ExecutionTarget)
	}
	if len(data.Attempts) != 1 || data.Attempts[0].VerificationStatus != "passed" || !data.Attempts[0].Winning {
		t.Fatalf("attempts = %#v", data.Attempts)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret verifier output", "secret task output", "true --with-secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("inspect task exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestInspectTaskRequiresExistingAttempt(t *testing.T) {
	fixture := buildRunFixture(t)
	_, err := InspectTask(t.Context(), InspectQuery{
		Workspace: fixture.workspace,
		RunID:     fixture.runID,
		TaskID:    fixture.taskID,
		Attempt:   99,
	})
	if err == nil {
		t.Fatal("missing attempt was accepted")
	}
}

func TestInspectRunDoesNotSearchSiblingBranches(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-main", "session-main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{ID: "main-start", Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "main"})}); err != nil {
		t.Fatal(err)
	}
	mainTerminal, err := store.AppendPersisted(team.RunEvent{ID: "main-finish", Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
		RunID: "run-main", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
	})})
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("feature")
	if _, err := store.AppendPersisted(team.RunEvent{ID: "feature-start", RunID: "run-feature", SessionID: "session-feature", Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "feature"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{ID: "feature-finish", RunID: "run-feature", SessionID: "session-feature", Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
		RunID: "run-feature", Outcome: team.RunOutcomeFailed, StopReason: team.StopReasonRunFailed,
	})}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree := team.NewSessionTree()
	tree.Branches["feature"] = &team.SessionBranch{
		ID: "feature", Name: "feature", ParentID: "main", ForkEventID: mainTerminal.ID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	tree.ActiveBranch = "main"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectRun(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-feature"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active branch lookup error = %v, want ErrNotFound", err)
	}
	envelope, err := InspectRun(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-feature", BranchID: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(RunData)
	if data.Outcome != string(team.RunOutcomeFailed) || envelope.Query.BranchID != "feature" {
		t.Fatalf("explicit branch run = %#v query=%#v", data, envelope.Query)
	}
}

func TestInspectRunSessionFilter(t *testing.T) {
	fixture := buildRunFixture(t)
	if _, err := InspectRun(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "wrong"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong session error = %v, want ErrNotFound", err)
	}
}

type runFixture struct {
	workspace string
	runID     string
	taskID    string
}

func buildRunFixture(t *testing.T) runFixture {
	t.Helper()
	workspace := t.TempDir()
	runID := "run-inspect"
	taskID := "task-inspect"
	store, err := team.NewEventStore(workspace, runID, "session-inspect")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	target := execution.ExecutionTarget{Backend: "ollama", Model: "frozen-model"}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "inspect"})})
	appendEvent(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: taskID, Payload: jsonBytes(t, map[string]any{
		"id": taskID, "status": team.TaskPending, "agent": "worker", "phase": "implementation",
		"execution_target": target, "execution_topology": []execution.ExecutionTarget{target},
	})})
	exitCode := 0
	receipt := team.ExecutionReceipt{
		RunID: runID, TaskID: taskID, Attempt: 1, Backend: "ollama",
		ModelExecutionID: "execution-1", ProducerID: "worker", ExitCode: &exitCode,
		TranscriptRef: "sha256-transcript",
		VerifyResult: &team.VerificationResult{
			Command: "true --with-secret", ExitCode: 0, Stdout: "secret verifier output", Fingerprint: "verify-fingerprint",
		},
	}
	appendEvent(team.RunEvent{Type: "task_completed", Actor: "worker", TaskID: taskID, Payload: jsonBytes(t, map[string]any{
		"id": taskID, "status": team.TaskDone, "agent": "worker", "phase": "implementation",
		"output": "secret task output", "execution_target": target,
		"execution_topology": []execution.ExecutionTarget{target}, "execution_receipts": []team.ExecutionReceipt{receipt},
	})})
	result := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomePartial, GoalSatisfied: false,
		StopReason: team.StopReasonUnresolvedTasks, Stats: team.RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
	}
	appendEvent(team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, result)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return runFixture{workspace: workspace, runID: runID, taskID: taskID}
}

func jsonBytes(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
