package team

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oversizedOutputsWorksetProvider writes a valid workset manifest and declares
// it, but returns more output keys than CanonicalizeRuntimeOutputs accepts.
type oversizedOutputsWorksetProvider struct{}

func (oversizedOutputsWorksetProvider) Validate(Action) error { return nil }

func (oversizedOutputsWorksetProvider) Execute(ctx context.Context, _ Action) (interface{}, error) {
	env := ActionEnvironmentFromContext(ctx)
	if err := os.WriteFile(filepath.Join(env.Workspace, "workset.json"), []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}}]}`), 0o644); err != nil {
		return nil, err
	}
	outputs := make(map[string]any, maxRuntimeOutputKeys+1)
	for i := 0; i <= maxRuntimeOutputKeys; i++ {
		outputs[fmt.Sprintf("key_%03d", i)] = i
	}
	return ActionResult{
		Outputs:   outputs,
		Artifacts: []ArtifactRef{{Path: "workset.json", Kind: "workset_manifest", Description: "workset-manifest"}},
	}, nil
}

func TestRuntimeActionRejectsOutputsBeforeIngestingArtifacts(t *testing.T) {
	session := workflowTestSession(t)
	registry := NewProviderRegistry()
	registry.Register("structured-actions", oversizedOutputsWorksetProvider{})
	session.ProviderRegistry = registry
	tracker := NewTaskTracker()
	c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: executePhaseWorkflow(t, session), executionRunID: "run-outputs-first"}
	task := TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]

	_, err := c.executeTask(context.Background(), task, item.ID)
	if err == nil || !strings.Contains(err.Error(), "canonicalize structured action outputs") {
		t.Fatalf("executeTask error = %v, want output canonicalization failure", err)
	}
	if item.TypedResult != nil {
		t.Fatalf("rejected action stored a typed result: %#v", item.TypedResult)
	}
	store, err := NewFileArtifactStore(session.Workspace, session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := store.ListByTask(context.Background(), item.ID); err != nil || len(refs) != 0 {
		t.Fatalf("ingested artifacts = %#v (err %v), want none", refs, err)
	}
	var pointers []string
	_ = filepath.WalkDir(session.Workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && entry.Name() == "current-workset.json" {
			pointers = append(pointers, path)
		}
		return nil
	})
	if len(pointers) != 0 {
		t.Fatalf("rejected action wrote workset pointers: %v", pointers)
	}
}
