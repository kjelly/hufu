package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMixedCaseTaskReferencePolicyMatchesRuntimeFactAndFanOut(t *testing.T) {
	staticTasks := []TaskDef{
		{ID: "Producer", Agent: "producer"},
		{ID: "consumer", Agent: "worker", FactRefs: []FactRef{{Name: "value", TaskID: "producer", Fact: "answer"}}},
		{ID: "review", Agent: "worker", FanOut: &FanOutSpec{
			SourceArtifact: FactRef{TaskID: "producer", Artifact: "manifest"},
			GoalTemplate:   "process {name}",
		}},
	}
	for _, finding := range ValidateTeamPolicyContracts(&TeamSession{ContractTasks: staticTasks}) {
		if finding.Severity == FindingSeverityError {
			t.Fatalf("preflight rejected mixed-case logical reference: %#v", finding)
		}
	}

	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "Producer", Agent: "producer", Desc: "produce"}})[0]
	producer.Status = TaskDone
	manifestBytes, err := json.Marshal(WorksetManifest{SchemaVersion: 1, Items: []WorksetItem{{
		Key: "one", Bindings: map[string]string{"name": "one"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Put(context.Background(), PutArtifactRequest{
		Content: manifestBytes, Path: "manifests/workset.json", Kind: "workset_manifest", RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		executionRunID: "run-1",
		taskTracker:    tracker,
		taskResults: map[string]*TaskResult{producer.ID: {
			TaskID:    producer.ID,
			Attempt:   1,
			Agent:     "producer",
			Status:    TaskResultStatusSuccess,
			Facts:     map[string]any{"answer": "resolved"},
			Artifacts: []ArtifactRef{manifest.ArtifactRef},
		}},
	}

	resolved, err := c.resolveFactRefs([]TaskDef{{
		Agent: "worker", Goal: "use {value}", FactRefs: []FactRef{{Name: "value", TaskID: "producer", Fact: "answer"}},
	}})
	if err != nil {
		t.Fatalf("mixed-case fact reference failed after preflight: %v", err)
	}
	if got := resolved[0].Goal; got != "use resolved" {
		t.Fatalf("resolved fact goal = %q, want %q", got, "use resolved")
	}

	expanded, err := c.expandFanOutTasks([]TaskDef{{
		Agent: "worker", FanOut: &FanOutSpec{
			SourceArtifact: FactRef{TaskID: "producer", Artifact: manifest.ID}, GoalTemplate: "process {name}",
		},
	}})
	if err != nil {
		t.Fatalf("mixed-case fan-out reference failed after preflight: %v", err)
	}
	if len(expanded) != 1 || expanded[0].Goal != "process one" || expanded[0].WorksetBinding == nil {
		t.Fatalf("expanded mixed-case fan-out = %#v", expanded)
	}
}

func TestMixedCaseTaskReferenceSurvivesTodoReplay(t *testing.T) {
	original := NewTaskTracker().TodoList()
	item := original.AddBatch([]TodoSpec{{PlanTaskID: "Producer", Agent: "producer", Desc: "produce"}})[0]
	replayed := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: item.ID, Payload: mustJSON(t, item)}})
	tracker := NewTaskTracker()
	tracker.TodoList().Restore(replayed)
	c := &Coordinator{
		taskTracker: tracker,
		taskResults: map[string]*TaskResult{item.ID: {TaskID: item.ID, Status: TaskResultStatusSuccess}},
	}
	runtimeID, err := c.resolveTaskReference("producer")
	if err != nil {
		t.Fatalf("mixed-case logical reference after replay: %v", err)
	}
	if runtimeID != item.ID {
		t.Fatalf("replayed runtime ID = %q, want %q", runtimeID, item.ID)
	}
}

func TestTaskReferenceKeepsExactRuntimeIDAndAmbiguityDetection(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "Producer", Agent: "one", Desc: "one"},
		{PlanTaskID: "producer", Agent: "two", Desc: "two"},
	})
	c := &Coordinator{taskTracker: tracker}
	if _, err := c.resolveTaskReference("PRODUCER"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("mixed-case logical ambiguity error = %v", err)
	}
	if got, err := c.resolveTaskReference(items[0].ID); err != nil || got != items[0].ID {
		t.Fatalf("exact runtime ID resolution = %q, %v", got, err)
	}
}
