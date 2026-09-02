package auditverify

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

// AC-6: audit explain must list every attempt and uniquely identify the
// winning attempt.
func TestExplainRunIdentifiesWinningAttempt(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	result, err := ExplainRun(context.Background(), fx.workspace, fx.runID)
	if err != nil {
		t.Fatalf("ExplainRun error: %v", err)
	}
	if result.Verification == nil || result.Verification.Verdict != AuditVerdictPass {
		t.Fatalf("verification = %#v, want pass", result.Verification)
	}
	if result.Witness == nil || result.Witness.WitnessHash == "" {
		t.Fatal("expected a sealed decision witness")
	}
	if err := result.Witness.Verify(); err != nil {
		t.Fatalf("witness does not self-verify: %v", err)
	}
	if len(result.Witness.Tasks) != 1 || result.Witness.Tasks[0].TaskID != fx.taskID {
		t.Fatalf("witness tasks = %#v, want exactly task %s", result.Witness.Tasks, fx.taskID)
	}
	if result.Witness.Tasks[0].WinningAttempt.ModelExecutionID != "exec-1" {
		t.Fatalf("winning attempt = %#v, want model_execution_id exec-1", result.Witness.Tasks[0].WinningAttempt)
	}

	if len(result.AttemptHistory) != 1 {
		t.Fatalf("attempt history = %#v, want exactly one task", result.AttemptHistory)
	}
	history := result.AttemptHistory[0]
	if len(history.Attempts) != 1 || !history.Attempts[0].Winning {
		t.Fatalf("attempt history = %#v, want a single winning attempt", history)
	}
}

func TestExplainRunLinksCriterionToAdvancingTask(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	result, err := ExplainRun(context.Background(), fx.workspace, fx.runID)
	if err != nil {
		t.Fatalf("ExplainRun error: %v", err)
	}
	if len(result.Witness.Criteria) != 1 {
		t.Fatalf("criteria = %#v, want exactly one", result.Witness.Criteria)
	}
	cw := result.Witness.Criteria[0]
	if cw.CriterionID != "build" {
		t.Fatalf("criterion id = %q, want build", cw.CriterionID)
	}
	if len(cw.EvidenceRequirementIDs) != 1 || cw.EvidenceRequirementIDs[0] != "task:"+fx.taskID {
		t.Fatalf("evidence requirement ids = %v, want [task:%s]", cw.EvidenceRequirementIDs, fx.taskID)
	}
	if len(cw.ArtifactIDs) != 1 || cw.ArtifactIDs[0] != fx.artifactID {
		t.Fatalf("artifact ids = %v, want [%s]", cw.ArtifactIDs, fx.artifactID)
	}
	if len(cw.ReceiptRefs) != 1 || cw.ReceiptRefs[0].ModelExecutionID != "exec-1" {
		t.Fatalf("receipt refs = %#v, want one ref to exec-1", cw.ReceiptRefs)
	}
}

func TestExplainRunMultipleAttemptsPicksLatestSuccessful(t *testing.T) {
	fx := buildRetriedTaskFixture(t)
	result, err := ExplainRun(context.Background(), fx.workspace, fx.runID)
	if err != nil {
		t.Fatalf("ExplainRun error: %v", err)
	}
	if len(result.AttemptHistory) != 1 {
		t.Fatalf("attempt history = %#v, want exactly one task", result.AttemptHistory)
	}
	history := result.AttemptHistory[0]
	if len(history.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want 2 (one failed, one verified)", history.Attempts)
	}
	if history.Attempts[0].Winning {
		t.Fatal("attempt 1 (failed) must not be the winner")
	}
	if !history.Attempts[1].Winning {
		t.Fatal("attempt 2 (verified) must be the winner")
	}
}

// AC-9: audit explain never calls an LLM. There is nothing in this package
// that can reach a provider/model client, so this test simply documents and
// pins that invariant against ExplainRun's public surface: it takes no model
// or provider argument, and its result contains no model-generated prose
// field for a caller to mistake as an LLM explanation.
func TestExplainRunSignatureHasNoModelInputs(t *testing.T) {
	var _ func(context.Context, string, string) (*ExplainResult, error) = ExplainRun
}

// buildRetriedTaskFixture builds a single-task completed run where the task
// needed two attempts: attempt 1 failed, attempt 2 was verified and won the
// evidence binding. This exercises the "retry task" attempt-history path
// (spec.md §25, AC-6) independently of buildCompletedRunFixture's single
// -attempt fixture.
func buildRetriedTaskFixture(t *testing.T) fixture {
	t.Helper()
	workspace := t.TempDir()
	runID := "run-fixture-retry"
	taskID := "t1"

	store, err := team.NewEventStore(workspace, runID, "session-fixture-retry")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()
	mustAppend := func(event team.RunEvent) {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}

	mustAppend(team.RunEvent{Type: "task_created", Actor: "coordinator", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "pending", Desc: "do the thing", Agent: "worker"})})

	artifactStore, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	putResult, err := artifactStore.Put(context.Background(), team.PutArtifactRequest{
		Kind: "task_transcript", Path: "transcript.txt", Content: []byte("attempt 2 transcript"),
		RunID: runID, TaskID: taskID, Attempt: 2, Agent: "worker",
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	transcriptRef := putResult.ArtifactRef

	failedExit := 1
	okExit := 0
	receipts := []team.ExecutionReceipt{
		{RunID: runID, TaskID: taskID, Attempt: 1, ModelExecutionID: "exec-1", ProducerID: "worker", ExitCode: &failedExit},
		{RunID: runID, TaskID: taskID, Attempt: 2, ModelExecutionID: "exec-2", ProducerID: "worker", ExitCode: &okExit, TranscriptRef: transcriptRef.ID},
	}
	mustAppend(team.RunEvent{Type: "task_completed", Actor: "worker", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "done", Desc: "do the thing", Agent: "worker", ExecutionReceipts: receipts})})

	manifest := &team.EvidenceManifest{
		RunID: runID, Status: "accepted",
		ArtifactRefs: []team.ArtifactRef{transcriptRef},
		EvidenceResults: []team.EvidenceResult{
			{
				RequirementID: "task:" + taskID, Status: "passed", Validator: "task-verification",
				ArtifactRefs: []team.ArtifactRef{transcriptRef},
				Binding: &team.EvidenceBinding{
					RunID: runID, TaskID: taskID, Attempt: 2, ModelExecutionID: "exec-2",
					ProducerID: "worker", TranscriptRef: transcriptRef.ID, ArtifactIDs: []string{transcriptRef.ID},
				},
			},
			{RequirementID: "run:acceptance", Status: "passed"},
		},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatalf("seal manifest: %v", err)
	}

	runResult := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true,
		StopReason: team.StopReasonCompleted, Acceptance: &team.AcceptanceResult{State: team.AcceptancePassed, Passed: true},
		EvidenceManifest: manifest,
	}
	mustAppend(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, runResult)})

	return fixture{workspace: workspace, runID: runID, taskID: taskID, artifactID: transcriptRef.ID}
}
