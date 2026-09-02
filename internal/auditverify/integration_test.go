package auditverify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// taskEventPayload mirrors the subset of internal/team's reducer wire format
// (event_reducers.go's task_* payload struct) this fixture needs. It exists
// only to author realistic RunEvent payloads from outside package team,
// without reaching into any unexported reducer function.
type taskEventPayload struct {
	ID                string                  `json:"id"`
	Status            string                  `json:"status"`
	Desc              string                  `json:"desc,omitempty"`
	Agent             string                  `json:"agent,omitempty"`
	Advances          []string                `json:"advances,omitempty"`
	ExecutionReceipts []team.ExecutionReceipt `json:"execution_receipts,omitempty"`
}

// fixture is a fully wired, canonical single-task completed run, built
// entirely through internal/team's exported append-only primitives (never
// through Coordinator internals, which are unexported and rightly so: a real
// audit bundle or workspace is exactly this kind of durable, tool-agnostic
// data, and the verifier must be able to work from it alone).
type fixture struct {
	workspace  string
	runID      string
	taskID     string
	artifactID string
}

func buildCompletedRunFixture(t *testing.T) fixture {
	t.Helper()
	workspace := t.TempDir()
	runID := "run-fixture-1"
	taskID := "t1"

	store, err := team.NewEventStore(workspace, runID, "session-fixture-1")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	mustAppend := func(event team.RunEvent) {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}

	mustAppend(team.RunEvent{Type: "run_started", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, map[string]any{"goal": "do the thing"})})

	mustAppend(team.RunEvent{Type: "task_created", Actor: "coordinator", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "pending", Desc: "do the thing", Agent: "worker"})})
	mustAppend(team.RunEvent{Type: "task_started", Actor: "coordinator", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "in_progress", Agent: "worker"})})

	artifactStore, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	putResult, err := artifactStore.Put(context.Background(), team.PutArtifactRequest{
		Kind: "task_transcript", Path: "transcript.txt", Content: []byte("worker transcript: task complete"),
		RunID: runID, TaskID: taskID, Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	transcriptRef := putResult.ArtifactRef

	receipt := team.ExecutionReceipt{
		RunID: runID, TaskID: taskID, Attempt: 1, ModelExecutionID: "exec-1",
		StartedAt: time.Now().UTC().Add(-time.Minute), FinishedAt: time.Now().UTC(),
		ExitCode: intPtr(0), ProducerID: "worker", TranscriptRef: transcriptRef.ID,
	}
	mustAppend(team.RunEvent{Type: "task_completed", Actor: "worker", RunID: runID, TaskID: taskID,
		Payload: mustJSON(t, taskEventPayload{ID: taskID, Status: "done", Desc: "do the thing", Agent: "worker", Advances: []string{"build"}, ExecutionReceipts: []team.ExecutionReceipt{receipt}})})

	manifest := &team.EvidenceManifest{
		RunID:        runID,
		Status:       "accepted",
		ArtifactRefs: []team.ArtifactRef{transcriptRef},
		EvidenceResults: []team.EvidenceResult{
			{
				RequirementID: "task:" + taskID, Status: "passed", Validator: "task-verification",
				ArtifactRefs: []team.ArtifactRef{transcriptRef},
				Binding: &team.EvidenceBinding{
					RunID: runID, TaskID: taskID, Attempt: 1, ModelExecutionID: "exec-1",
					ProducerID: "worker", TranscriptRef: transcriptRef.ID, ArtifactIDs: []string{transcriptRef.ID},
				},
				CheckedAt: time.Now().UTC(),
			},
			{RequirementID: "run:acceptance", Status: "passed", Validator: "acceptance-gate", CheckedAt: time.Now().UTC()},
		},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatalf("seal manifest: %v", err)
	}
	if err := manifest.Verify(context.Background(), artifactStore); err != nil {
		t.Fatalf("fixture manifest does not self-verify: %v", err)
	}

	acceptance := &team.AcceptanceResult{
		State: team.AcceptancePassed, Passed: true,
		CriterionResults: []team.CriterionResult{{ID: "build", State: team.CriterionPassed, EvaluatedAt: time.Now().UTC()}},
	}

	runResult := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true,
		StopReason: team.StopReasonCompleted, ExitCode: 0,
		Acceptance: acceptance, EvidenceManifest: manifest,
		Stats: team.RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
	}
	mustAppend(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, runResult)})

	return fixture{workspace: workspace, runID: runID, taskID: taskID, artifactID: transcriptRef.ID}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture payload: %v", err)
	}
	return b
}

func intPtr(v int) *int { return &v }

func TestVerifyWorkspaceRunPassesOnCompletedFixture(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun error: %v", err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("verdict = %s, want pass; result=%#v", result.Verdict, result)
	}
	for name, dim := range map[string]AuditDimensionResult{
		"integrity": result.Integrity, "provenance": result.Provenance, "evidence": result.Evidence,
		"acceptance": result.Acceptance, "completion": result.Completion,
	} {
		if dim.Status != AuditDimensionPass {
			t.Fatalf("dimension %s = %#v, want pass", name, dim)
		}
	}
	if result.ExpectedOutcome != team.RunOutcomeCompleted || result.DerivedOutcome != team.RunOutcomeCompleted {
		t.Fatalf("outcomes = expected=%s derived=%s, want completed/completed", result.ExpectedOutcome, result.DerivedOutcome)
	}
}

func TestVerifyWorkspaceRunUnknownRunIsError(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	if _, err := VerifyWorkspaceRun(context.Background(), fx.workspace, "run-does-not-exist", VerifyOptions{}); err == nil {
		t.Fatal("expected an error for an unknown run id")
	}
}

// AC-2: mutating any canonical event payload on disk makes audit FAIL.
func TestVerifyWorkspaceRunFailsOnMutatedEventPayload(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	path := filepath.Join(fx.workspace, "logs", "event_store.jsonl")
	tamperLine(t, path, func(line string) string {
		return strings.Replace(line, `"task_created"`, `"tampered_type"`, 1)
	}, 1)

	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun error: %v", err)
	}
	if result.Integrity.Status != AuditDimensionFail || result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered event payload result = %#v, want integrity fail / verdict fail", result)
	}
}

// AC-3: modifying an evidence artifact's bytes makes audit FAIL.
func TestVerifyWorkspaceRunFailsOnTamperedArtifact(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	path := filepath.Join(fx.workspace, "logs", "artifacts", "data", fx.artifactID)
	if err := os.WriteFile(path, []byte("tampered bytes"), 0o644); err != nil {
		t.Fatalf("tamper artifact: %v", err)
	}

	result, err := VerifyWorkspaceRun(context.Background(), fx.workspace, fx.runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun error: %v", err)
	}
	if result.Evidence.Status != AuditDimensionFail || result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered artifact result = %#v, want evidence fail / verdict fail", result)
	}
}

// AC-4: a completed run whose evidence manifest is missing entirely (never
// sealed) must FAIL, not silently pass.
func TestVerifyWorkspaceRunFailsWhenCompletedWithoutEvidenceManifest(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-no-manifest"
	store, err := team.NewEventStore(workspace, runID, "session-no-manifest")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	runResult := team.RunResult{RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true,
		StopReason: team.StopReasonCompleted, Acceptance: &team.AcceptanceResult{State: team.AcceptancePassed, Passed: true}}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, runResult)}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	result, err := VerifyWorkspaceRun(context.Background(), workspace, runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun error: %v", err)
	}
	if result.Evidence.Status != AuditDimensionFail || result.Verdict != AuditVerdictFail {
		t.Fatalf("completed-without-manifest result = %#v, want evidence fail / verdict fail", result)
	}
}

// AC-5: a legitimately failed run with a fully consistent canonical record
// must audit PASS -- AuditVerdict is not RunOutcome (spec.md §50).
func TestVerifyWorkspaceRunPassesOnLegitimateFailure(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-legit-failure"
	store, err := team.NewEventStore(workspace, runID, "session-legit-failure")
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	defer func() { _ = store.Close() }()

	runResult := team.RunResult{RunID: runID, Outcome: team.RunOutcomeFailed, GoalSatisfied: false,
		StopReason: team.StopReasonRunFailed, ExitCode: 1, Reason: "worker crashed"}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_finished", Actor: "coordinator", RunID: runID, Payload: mustJSON(t, runResult)}); err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	result, err := VerifyWorkspaceRun(context.Background(), workspace, runID, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyWorkspaceRun error: %v", err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("legitimate failure result = %#v, want verdict pass", result)
	}
	if result.ExpectedOutcome != team.RunOutcomeFailed || result.DerivedOutcome != team.RunOutcomeFailed {
		t.Fatalf("outcomes = expected=%s derived=%s, want failed/failed", result.ExpectedOutcome, result.DerivedOutcome)
	}
}

// tamperLine rewrites the nth line (0-indexed) of a JSONL file using fn, then
// rewrites the whole file. It is deliberately dumb text surgery: the point is
// to corrupt a persisted byte, not to author a new valid event.
func tamperLine(t *testing.T, path string, fn func(string) string, n int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(trimmed, "\n")
	if n >= len(lines) {
		t.Fatalf("tamperLine: line %d out of range (%d lines)", n, len(lines))
	}
	lines[n] = fn(lines[n])
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
