package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWP06GenericWorksetFixtures is the migration gate's two consumer-neutral
// fixtures. They exercise the same runtime contract with different producer
// payloads: scalar transformation inputs and read-only probe inputs.
func TestWP06GenericWorksetFixtures(t *testing.T) {
	t.Run("transform", func(t *testing.T) {
		workspace := t.TempDir()
		writeFixtureManifest(t, workspace, WorksetManifest{
			SchemaVersion: WorksetSchemaVersion,
			Items: []WorksetItem{
				{Key: "alpha", Bindings: map[string]string{"input": "alpha.txt"}},
				{Key: "beta", Bindings: map[string]string{"input": "beta.txt"}},
				{Key: "gamma", Bindings: map[string]string{"input": "gamma.txt"}},
			},
		})
		c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "fixture-transform"}
		expanded, err := c.expandFanOutTasks([]TaskDef{
			{ID: "producer", Agent: "worker", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "transform {input}"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertFixtureExpansion(t, expanded, []string{"alpha", "beta", "gamma"}, []string{"transform alpha.txt", "transform beta.txt", "transform gamma.txt"})
		assertFixtureReceipt(t, expanded, []string{"child-alpha", "child-beta", "child-gamma"}, "fixture-transform")
	})

	t.Run("probe", func(t *testing.T) {
		workspace := t.TempDir()
		store, err := NewFileArtifactStore(workspace, workspace)
		if err != nil {
			t.Fatal(err)
		}
		inputs := make([]ArtifactRef, 0, 3)
		for _, key := range []string{"alpha", "beta", "gamma"} {
			input, putErr := store.Put(context.Background(), PutArtifactRequest{
				Content: []byte("read-only target " + key), Path: "inputs/" + key + ".txt", Kind: "probe_input",
				RunID: "fixture-probe", TaskID: "producer",
			})
			if putErr != nil {
				t.Fatal(putErr)
			}
			inputs = append(inputs, input)
		}
		manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: make([]WorksetItem, 0, len(inputs))}
		for index, input := range inputs {
			key := []string{"alpha", "beta", "gamma"}[index]
			manifest.Items = append(manifest.Items, WorksetItem{
				Key: key, Bindings: map[string]string{"target": key}, Inputs: []ArtifactRef{input},
			})
		}
		manifestBytes, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifestRef, err := store.Put(context.Background(), PutArtifactRequest{
			Content: manifestBytes, Path: "manifests/probe.json", Kind: "workset_manifest",
			RunID: "fixture-probe", TaskID: "producer",
		})
		if err != nil {
			t.Fatal(err)
		}
		c := &Coordinator{
			session: &TeamSession{Workspace: workspace}, executionRunID: "fixture-probe",
			taskResults: map[string]*TaskResult{"producer": {TaskID: "producer", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{manifestRef}}},
		}
		expanded, err := c.expandFanOutTasks([]TaskDef{{ID: "producer", Agent: "worker", FanOut: &FanOutSpec{
			SourceArtifact: FactRef{TaskID: "producer", Artifact: manifestRef.ID}, GoalTemplate: "probe {target}",
		}}})
		if err != nil {
			t.Fatal(err)
		}
		assertFixtureExpansion(t, expanded, []string{"alpha", "beta", "gamma"}, []string{"probe alpha", "probe beta", "probe gamma"})
		for index, child := range expanded {
			if child.WorksetBinding == nil || len(child.WorksetBinding.Inputs) != 1 || child.WorksetBinding.Inputs[0].ID != inputs[index].ID {
				t.Fatalf("probe child %d lost opaque input binding: %#v", index, child.WorksetBinding)
			}
		}
		assertFixtureReceipt(t, expanded, []string{"probe-alpha", "probe-beta", "probe-gamma"}, "fixture-probe")

		// A single failed child must fail the generic group gate; no consumer
		// wording or presentation output participates in this assertion.
		tracker := NewTaskTracker()
		items := tracker.TodoList().AddBatch([]TodoSpec{
			{PlanTaskID: "probe-alpha", Agent: "worker", WorksetBinding: cloneWorksetBinding(expanded[0].WorksetBinding)},
			{PlanTaskID: "probe-beta", Agent: "worker", WorksetBinding: cloneWorksetBinding(expanded[1].WorksetBinding)},
			{PlanTaskID: "probe-gamma", Agent: "worker", WorksetBinding: cloneWorksetBinding(expanded[2].WorksetBinding)},
		})
		ids := make([]string, len(items))
		for index, item := range items {
			ids[index] = item.ID
			item.Status = TaskDone
			item.VerifyResult = &VerificationResult{ExitCode: 0}
			status := TaskResultStatusSuccess
			if index == 1 {
				status = TaskResultStatusPartial
			}
			item.TypedResult = &TaskResult{TaskID: item.ID, Status: status, Summary: "fixture result", Source: "submitted"}
		}
		receipts, err := buildWorksetReceipts(expanded, ids, "fixture-probe")
		if err != nil {
			t.Fatal(err)
		}
		receipt := receipts[expanded[0].WorksetBinding.WorksetID]
		items[0].WorksetReceipt = receipt
		c.taskTracker = tracker
		result, verifyErr := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
			Type: VerifyWorksetComplete, WorksetSourceTask: "producer", WorksetRequireTerminal: true, WorksetRequireVerified: true,
		})
		if verifyErr == nil || result == nil || result.ExitCode == 0 {
			t.Fatalf("probe failure fixture unexpectedly passed: result=%#v err=%v", result, verifyErr)
		}
	})
}

func writeFixtureManifest(t *testing.T, workspace string, manifest WorksetManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFixtureExpansion(t *testing.T, tasks []TaskDef, keys, goals []string) {
	t.Helper()
	if len(tasks) != len(keys) || len(tasks) != len(goals) {
		t.Fatalf("expanded %d task(s), want %d: %#v", len(tasks), len(keys), tasks)
	}
	for index, task := range tasks {
		if task.WorksetBinding == nil || task.WorksetBinding.ItemKey != keys[index] || task.Goal != goals[index] {
			t.Fatalf("expanded[%d] = %#v, want key=%q goal=%q", index, task, keys[index], goals[index])
		}
	}
}

func assertFixtureReceipt(t *testing.T, tasks []TaskDef, childIDs []string, runID string) {
	t.Helper()
	ids := make([]string, len(childIDs))
	copy(ids, childIDs)
	receipts, err := buildWorksetReceipts(tasks, ids, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("got %d workset receipts, want 1: %#v", len(receipts), receipts)
	}
	for _, receipt := range receipts {
		if receipt.ItemCount != 3 || len(receipt.Children) != 3 || receipt.RunID != runID {
			t.Fatalf("fixture receipt = %#v", receipt)
		}
	}
}
