package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorksetManifest(t *testing.T, workspace string, content string) string {
	t.Helper()
	path := filepath.Join(workspace, "manifest.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStructuredFanOutUsesValidatedManifestBindings(t *testing.T) {
	workspace := t.TempDir()
	manifestPath := writeWorksetManifest(t, workspace, `{"schema_version":1,"items":[{"key":"alpha","bindings":{"name":"alpha","input":"a.txt"}},{"key":"beta","bindings":{"name":"beta","input":"b.txt"}}]}`)
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	parent := TaskDef{ID: "prepare", Agent: "worker", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "process {name} from {input}"}}
	tasks, err := c.expandFanOutTasks([]TaskDef{parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Goal != "process alpha from a.txt" || tasks[1].WorksetBinding == nil {
		t.Fatalf("structured fan-out = %#v", tasks)
	}
	if tasks[0].WorksetBinding.ItemKey != "alpha" || tasks[1].WorksetBinding.ItemKey != "beta" || tasks[0].WorksetBinding.SourceArtifactID == "" {
		t.Fatalf("missing immutable workset binding: %#v", tasks)
	}
	if tasks[0].WorksetBinding.WorksetID != tasks[1].WorksetBinding.WorksetID {
		t.Fatal("children do not share one workset identity")
	}
	originalSource := tasks[0].WorksetBinding.SourceSHA256
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"items":[{"key":"changed","bindings":{"name":"changed","input":"c.txt"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	receipts, err := buildWorksetReceipts(tasks, []string{"child-1", "child-2"}, "run-1")
	if err != nil || receipts[tasks[0].WorksetBinding.WorksetID].SourceSHA256 != originalSource {
		t.Fatalf("committed workset changed after source mutation: receipts=%#v err=%v", receipts, err)
	}
}

func TestStructuredFanOutRejectsInvalidManifestAtomically(t *testing.T) {
	for name, content := range map[string]string{
		"duplicate key": `{"schema_version":1,"items":[{"key":"same","bindings":{"x":"1"}},{"key":"same","bindings":{"x":"2"}}]}`,
		"empty key":     `{"schema_version":1,"items":[{"key":"","bindings":{"x":"1"}}]}`,
		"bad schema":    `{"schema_version":2,"items":[{"key":"one","bindings":{"x":"1"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			writeWorksetManifest(t, workspace, content)
			c := &Coordinator{session: &TeamSession{Workspace: workspace}}
			if expanded, err := c.expandFanOutTasks([]TaskDef{
				{Agent: "other", Goal: "must not commit"},
				{Agent: "worker", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "{x}"}},
			}); err == nil || expanded != nil {
				t.Fatalf("invalid manifest was partially expanded: expanded=%#v err=%v", expanded, err)
			}
		})
	}
}

func TestStructuredFanOutResolvesOpaqueSourceArtifactAndInputIntegrity(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("input"), Path: "inputs/input.txt", Kind: "input", RunID: "run-1", TaskID: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := json.Marshal(WorksetManifest{SchemaVersion: 1, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{input}}}})
	manifest, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifests/workset.json", Kind: "workset_manifest", RunID: "run-1", TaskID: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		executionRunID: "run-1",
		taskResults:    map[string]*TaskResult{"producer": {TaskID: "producer", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{manifest}}},
	}
	tasks, err := c.expandFanOutTasks([]TaskDef{{Agent: "worker", FanOut: &FanOutSpec{
		SourceArtifact: FactRef{TaskID: "producer", Artifact: manifest.ID}, GoalTemplate: "process {name}",
	}}})
	if err != nil || len(tasks) != 1 || tasks[0].WorksetBinding == nil || len(tasks[0].WorksetBinding.Inputs) != 1 {
		t.Fatalf("opaque source artifact expansion = %#v err=%v", tasks, err)
	}
}

func TestWorksetReceiptBindsEveryChildAndReplays(t *testing.T) {
	tasks := []TaskDef{
		{WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "a", SourceArtifactID: "artifact-1", SourceSHA256: strings.Repeat("a", 64)}},
		{WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "b", SourceArtifactID: "artifact-1", SourceSHA256: strings.Repeat("a", 64)}},
	}
	receipts, err := buildWorksetReceipts(tasks, []string{"11", "12"}, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	receipt := receipts["workset-1"]
	if receipt == nil || receipt.ItemCount != 2 || receipt.Children["a"] != "11" || receipt.Children["b"] != "12" {
		t.Fatalf("receipt = %#v", receipt)
	}
	raw, _ := json.Marshal(map[string]any{"id": "11", "status": "pending", "workset_binding": tasks[0].WorksetBinding, "workset_receipt": receipt})
	projected := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "11", Payload: raw}})
	if len(projected) != 1 || projected[0].WorksetBinding == nil || projected[0].WorksetReceipt == nil || projected[0].WorksetReceipt.Children["b"] != "12" {
		t.Fatalf("replayed workset projection = %#v", projected)
	}
}
