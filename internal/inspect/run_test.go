package inspect

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
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
	if len(data.Attempts) != 1 || data.Attempts[0].ExecutionTarget != "ollama/frozen-model" || data.Attempts[0].VerificationStatus != "passed" || !data.Attempts[0].Winning {
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

func TestProjectTaskUsesAttemptAnchoredTargetsAndBothTranscriptRefs(t *testing.T) {
	exitCode := 0
	first := team.ExecutionReceipt{
		RunID: "run-1", TaskID: "task-1", Attempt: 1, Backend: "ollama",
		ModelExecutionID: "execution-fast", ProducerID: "worker", ExitCode: &exitCode,
		TranscriptRef: "task-transcript", ProviderTranscriptRef: "provider-transcript",
	}
	second := team.ExecutionReceipt{
		RunID: "run-1", TaskID: "task-1", Attempt: 2, Backend: "ollama",
		ModelExecutionID: "execution-strong", ProducerID: "worker", ExitCode: &exitCode,
	}
	item := &team.TodoItem{
		ID: "task-1", ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "strong"},
		ExecutionReceipts: []team.ExecutionReceipt{first, second},
	}
	events := []IndexedEvent{
		{Ordinal: 1, Event: team.RunEvent{ID: "event-fast", TaskID: item.ID, Payload: jsonBytes(t, map[string]any{
			"execution_target": execution.ExecutionTarget{Backend: "ollama", Model: "fast"}, "execution_receipts": []team.ExecutionReceipt{first},
		})}},
		{Ordinal: 2, Event: team.RunEvent{ID: "event-strong", TaskID: item.ID, Payload: jsonBytes(t, map[string]any{
			"execution_target": execution.ExecutionTarget{Backend: "ollama", Model: "strong"}, "execution_receipts": []team.ExecutionReceipt{second},
		})}},
	}
	query := InspectQuery{RunID: "run-1", TaskID: item.ID}
	data := projectTaskWithEvents(item, query, events)
	if len(data.Attempts) != 2 || data.Attempts[0].ExecutionTarget != "ollama/fast" || data.Attempts[1].ExecutionTarget != "ollama/strong" {
		t.Fatalf("attempt targets = %#v", data.Attempts)
	}
	for _, ref := range []string{"task-transcript", "provider-transcript"} {
		if !slices.Contains(data.ArtifactRefs, ref) {
			t.Fatalf("artifact refs %v do not contain %q", data.ArtifactRefs, ref)
		}
	}
	trace := receiptTraceCandidates(events, item, query)
	if len(trace) != 2 || trace[0].entry.Ref.ExecutionTarget != "ollama/fast" || trace[1].entry.Ref.ExecutionTarget != "ollama/strong" {
		t.Fatalf("trace attempt targets = %#v", trace)
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

func TestInspectEvidenceUsesAuditVerificationAndHidesRawFields(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectEvidence(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(EvidenceData)
	if data.Manifest.Hash == "" || len(data.Requirements) != 1 || data.Requirements[0].Binding == nil {
		t.Fatalf("evidence data = %#v", data)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret verifier output", "secret task output", "true --with-secret", "/private/artifact/path"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("inspect evidence exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestInspectContextSeparatesManifestsAndEnforcesPrivateScope(t *testing.T) {
	fixture := buildRunFixture(t)
	repo, err := contextstore.OpenSQLite(filepath.Join(fixture.workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "private-context", Kind: contextstore.ContextPattern, Content: "safe detail password=do-not-show",
		Scope:     contextstore.Scope{ProjectID: "project-1", TeamID: "team-1", AgentID: "worker"},
		Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal, Lifecycle: contextstore.LifecycleConfirmed,
		Source: contextstore.SourceRef{Type: "runtime", Ref: "Authorization: Bearer do-not-show"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, TaskID: fixture.taskID, ProjectID: "project-1", TeamID: "team-1"}
	if _, err := InspectContext(t.Context(), query, ContextOptions{ShowContent: true}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("private context without agent error = %v", err)
	}
	query.AgentID = "worker"
	envelope, err := InspectContext(t.Context(), query, ContextOptions{ShowContent: true})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ContextData)
	if len(data.Manifests) != 1 || len(data.MemoryManifests) != 1 || data.MemoryManifests[0].RetrievalID != "retrieval-1" {
		t.Fatalf("context manifests = %#v / %#v", data.Manifests, data.MemoryManifests)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "do-not-show") {
		t.Fatalf("context output leaked secret: %s", encoded)
	}
}

func TestInspectContextHidesContentByDefault(t *testing.T) {
	fixture := buildRunFixture(t)
	repo, err := contextstore.OpenSQLite(filepath.Join(fixture.workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	const privateContent = "private context content without credential syntax"
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "private-context", Kind: contextstore.ContextPattern, Content: privateContent,
		Scope: contextstore.Scope{ProjectID: "project-1", TeamID: "team-1", AgentID: "worker"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	envelope, err := InspectContext(t.Context(), InspectQuery{
		Workspace: fixture.workspace, RunID: fixture.runID, TaskID: fixture.taskID,
		ProjectID: "project-1", TeamID: "team-1", AgentID: "worker",
	}, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ContextData)
	if len(data.Items) != 1 || data.Items[0].Content != "" {
		t.Fatalf("default context projection = %#v", data.Items)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), privateContent) {
		t.Fatalf("default context projection exposed content: %s", encoded)
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
	contextManifest := team.ContextInjectionManifest{
		SchemaVersion: 1, RequestID: "request-1", RunID: runID, TaskID: taskID, Attempt: 1,
		Agent: "worker", ModelExecutionID: "execution-1", Fingerprint: "context-fingerprint",
		Items: []team.ContextManifestItem{{ID: "private-context", Included: true, Tokens: 8, Reason: team.ContextIncludedRelevant}},
	}
	memoryManifest := team.MemoryInjectionManifest{
		RetrievalID: "retrieval-1", RunID: runID, TaskID: taskID, Attempt: 1, Agent: "worker", PolicyVersion: "policy-v1", Fingerprint: "memory-fingerprint",
		Items: []team.MemoryInjectionItem{{ContextItemID: "private-context", Rank: 1, TokenCount: 8, BaseScore: 0.8, FinalScore: 0.7}},
	}
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
		"context_manifests": []team.ContextInjectionManifest{contextManifest}, "memory_manifests": []team.MemoryInjectionManifest{memoryManifest},
	})})
	manifest := &team.EvidenceManifest{
		RunID: runID, Status: "accepted", ArtifactRefs: []team.ArtifactRef{{ID: "artifact-1", SHA256: "digest-1", Path: "/private/artifact/path"}},
		EvidenceResults: []team.EvidenceResult{{RequirementID: "req-1", Status: "passed", Validator: "receipt", Binding: &team.EvidenceBinding{
			RunID: runID, TaskID: taskID, Attempt: 1, ModelExecutionID: "execution-1", ProducerID: "worker", TranscriptRef: "sha256-transcript", ArtifactIDs: []string{"artifact-1"},
		}}},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	result := team.RunResult{
		RunID: runID, Outcome: team.RunOutcomePartial, GoalSatisfied: false,
		StopReason: team.StopReasonUnresolvedTasks, Stats: team.RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
		EvidenceManifest: manifest,
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
