package team

import (
	"context"
	"encoding/json"
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
	if err := os.WriteFile(filepath.Join(workspace, "one.json"), []byte(`{"one":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "two.json"), []byte(`{"two":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	refs, err := c.ingestActionProviderArtifacts(context.Background(), TaskDef{Agent: "producer"}, "task-1", 3, []ArtifactRef{
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
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	for _, path := range []string{"missing.json", "../outside.json"} {
		t.Run(path, func(t *testing.T) {
			if _, err := c.ingestActionProviderArtifacts(context.Background(), TaskDef{Agent: "producer"}, "task-1", 1, []ArtifactRef{{Path: path}}); err == nil {
				t.Fatalf("path %q unexpectedly accepted", path)
			}
		})
	}
}
