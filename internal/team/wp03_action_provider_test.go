package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoordinatorRuntimeActionPersistsProviderArtifactsAsTypedEvidence(t *testing.T) {
	session := workflowTestSession(t)
	if err := os.WriteFile(filepath.Join(session.Workspace, "result.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &recordingActionProvider{result: ActionResult{
		Outputs:   map[string]any{"count": 3},
		Artifacts: []ArtifactRef{{ID: "provider-forged", Path: "result.json", SHA256: strings.Repeat("0", 64)}},
	}}
	registry := NewProviderRegistry()
	registry.Register("structured-actions", provider)
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	task := TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	events, err := NewEventStore(session.Workspace, "run-action-artifact", "session-action-artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: w, eventStore: events, executionRunID: "run-action-artifact"}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatal(err)
	}
	got := tracker.TodoList().Items()[0]
	if got.TypedResult == nil || len(got.TypedResult.Artifacts) != 1 || got.TypedResult.Artifacts[0].ID == "provider-forged" {
		t.Fatalf("typed provider artifact = %#v", got.TypedResult)
	}
	if got.TypedResult.Facts["count"] != 3 {
		t.Fatalf("typed provider outputs = %#v", got.TypedResult.Facts)
	}
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var artifactEvent RunEvent
	for _, event := range stored {
		if event.Type == "artifact_created" {
			artifactEvent = event
			break
		}
	}
	if artifactEvent.Type == "" {
		t.Fatal("provider artifact_created event missing")
	}
	var payload struct {
		Artifact ArtifactRef `json:"artifact"`
	}
	if err := json.Unmarshal(artifactEvent.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Artifact.Provider != "fake-action-adapter" || payload.Artifact.RunID != "run-action-artifact" {
		t.Fatalf("artifact event provenance = %#v", payload.Artifact)
	}
}

func TestDecodeActionResultEnvelopeAndRejectsUndeclaredFields(t *testing.T) {
	result, err := decodeActionResult(map[string]any{
		"outputs": map[string]any{"count": float64(3)},
		"artifacts": []any{
			map[string]any{"path": "one.json"},
			map[string]any{"path": "two.json"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["count"] != float64(3) || len(result.Artifacts) != 2 {
		t.Fatalf("decoded action result = %#v", result)
	}
	if _, err := decodeActionResult(map[string]any{"outputs": map[string]any{}, "unexpected": true}); err == nil {
		t.Fatal("undeclared action result field unexpectedly accepted")
	}
}

func TestIngestActionProviderArtifactsRecomputesIdentityAndProvenance(t *testing.T) {
	workspace := t.TempDir()
	actionRoot := filepath.Join(workspace, "runtime", "runs", "run-1", "actions", "action-1")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "one.json"), []byte(`{"one":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "two.json"), []byte(`{"two":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	refs, err := c.ingestActionProviderArtifacts(context.Background(), actionRoot, TaskDef{Agent: "producer"}, "task-1", 3, []ArtifactRef{
		{ID: "forged", Path: "one.json", SHA256: strings.Repeat("0", 64)},
		{Path: "two.json", Description: "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID == "forged" || refs[0].SHA256 == strings.Repeat("0", 64) {
		t.Fatalf("runtime did not recompute artifact identity: %#v", refs)
	}
	for _, ref := range refs {
		if ref.RunID != "run-1" || ref.TaskID != "task-1" || ref.Agent != "producer" || ref.Attempt != 3 {
			t.Fatalf("artifact provenance = %#v", ref)
		}
	}
}

func TestIngestActionProviderArtifactsRejectsMissingAndEscapedPaths(t *testing.T) {
	workspace := t.TempDir()
	actionRoot := filepath.Join(workspace, "runtime", "runs", "run-1", "actions", "action-1")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	insideSession := filepath.Join(workspace, "stable.json")
	if err := os.WriteFile(insideSession, []byte("durable"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	for _, path := range []string{"missing.json", "../outside.json", insideSession} {
		t.Run(path, func(t *testing.T) {
			if _, err := c.ingestActionProviderArtifacts(context.Background(), actionRoot, TaskDef{Agent: "producer"}, "task-1", 1, []ArtifactRef{{Path: path}}); err == nil {
				t.Fatalf("path %q unexpectedly accepted", path)
			}
		})
	}
}

func newCommandActionCoordinator(t *testing.T, command []string, runID string) (*Coordinator, *TeamSession) {
	t.Helper()
	session := workflowTestSession(t)
	registry := NewProviderRegistry()
	registry.Register("structured-actions", &commandActionProvider{capability: "structured-actions", command: command})
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	return &Coordinator{session: session, taskTracker: NewTaskTracker(), phaseWorkflow: w, executionRunID: runID}, session
}

func TestRuntimeActionCommandProviderGetsFreshNamespacesAndPreservesEvidence(t *testing.T) {
	command := []string{"/bin/sh", "-c", `set -eu
if [ -n "$(find "$HUFU_WORKSPACE" -mindepth 1 -print -quit)" ]; then
  echo "provider workspace is not empty" >&2
  exit 41
fi
printf '%s' "$HUFU_ACTION_INVOCATION_ID" > "$HUFU_WORKSPACE/artifact.txt"
printf '{"outputs":{"action_id":"%s"},"artifacts":[{"path":"artifact.txt","kind":"provider-output"}]}' "$HUFU_ACTION_INVOCATION_ID"
`}
	c, session := newCommandActionCoordinator(t, command, "run-command")
	oldEvidence := filepath.Join(session.Workspace, "workset", "workset-manifest.json")
	if err := os.MkdirAll(filepath.Dir(oldEvidence), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldEvidence, []byte("old evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "action-1", Phase: PhaseExecute, ContractID: "execute-1", Action: task.Action, Agent: task.Agent, Desc: task.Goal},
		{PlanTaskID: "action-2", Phase: PhaseExecute, ContractID: "execute-2", Action: task.Action, Agent: task.Agent, Desc: task.Goal},
	})
	for _, item := range items {
		if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
			t.Fatalf("executeRuntimeAction(%s): %v", item.ID, err)
		}
	}
	roots, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "runs", "run-command", "actions", "structured-action-*"))
	if err != nil || len(roots) != 2 {
		t.Fatalf("action roots = %v, err=%v; want two unique roots", roots, err)
	}
	refs := make([]ArtifactRef, 0, 2)
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.TypedResult == nil || len(item.TypedResult.Artifacts) != 1 {
			t.Fatalf("typed result for %s = %#v", item.ID, item.TypedResult)
		}
		ref := item.TypedResult.Artifacts[0]
		if !strings.HasPrefix(ref.Path, "runtime/runs/run-command/actions/structured-action-") || ref.RunID != "run-command" || ref.TaskID != item.ID || ref.Attempt != 1 {
			t.Fatalf("artifact provenance for %s = %#v", item.ID, ref)
		}
		refs = append(refs, ref)
	}
	if refs[0].ID == refs[1].ID || refs[0].Path == refs[1].Path {
		t.Fatalf("artifact identities or paths were reused: %#v", refs)
	}
	if content, err := os.ReadFile(oldEvidence); err != nil || string(content) != "old evidence" {
		t.Fatalf("old evidence changed: content=%q err=%v", content, err)
	}
}

func TestRuntimeActionRetryKeepsFailedResidueAndAllocatesNewNamespace(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "provider-marker")
	command := []string{"/bin/sh", "-c", fmt.Sprintf(`set -eu
if [ ! -f %q ]; then
  printf partial > "$HUFU_WORKSPACE/partial.txt"
  touch %q
  exit 42
fi
printf success > "$HUFU_WORKSPACE/artifact.txt"
printf '{"artifacts":[{"path":"artifact.txt","kind":"provider-output"}]}'
`, marker, marker)}
	c, session := newCommandActionCoordinator(t, command, "run-retry")
	task := TaskDef{Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "retry", Phase: PhaseExecute, ContractID: "execute", Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err == nil {
		t.Fatal("failed provider attempt unexpectedly succeeded")
	}
	firstRoots, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "runs", "run-retry", "actions", "structured-action-*"))
	if err != nil || len(firstRoots) != 1 {
		t.Fatalf("failed-attempt roots = %v, err=%v", firstRoots, err)
	}
	if _, err := os.Stat(filepath.Join(firstRoots[0], "partial.txt")); err != nil {
		t.Fatalf("failed-attempt residue missing: %v", err)
	}
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry provider action"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("retry provider attempt failed: %v", err)
	}
	allRoots, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "runs", "run-retry", "actions", "structured-action-*"))
	if err != nil || len(allRoots) != 2 || allRoots[0] == allRoots[1] {
		t.Fatalf("retry roots = %v, err=%v; want two preserved distinct roots", allRoots, err)
	}
}
