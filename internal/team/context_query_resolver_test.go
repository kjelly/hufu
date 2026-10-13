package team

import (
	json "encoding/json/v2"
	"slices"
	"testing"
)

func TestContextQueryResolverUsesAcceptedCurrentRunDependenciesOnly(t *testing.T) {
	c := &Coordinator{executionRunID: "run", taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "source"}, {Agent: "worker", Desc: "foreign"}, {Agent: "worker", Desc: "pending"}, {Agent: "worker", Desc: "consumer"}})
	for index, source := range items[:3] {
		source.Status = TaskDone
		source.TypedResult = &TaskResult{TaskID: source.ID, Attempt: 1, Status: TaskResultStatusSuccess, Summary: "summary " + source.ID}
		source.ExecutionReceipt = &ExecutionReceipt{RunID: "run", TaskID: source.ID, Attempt: 1}
		if index == 1 {
			source.ExecutionReceipt.RunID = "foreign-run"
		}
		if index == 2 {
			source.Status = TaskInProgress
		}
	}
	consumer := items[3]
	consumer.DependsOn = []string{items[2].ID, items[1].ID}
	consumer.EvidenceFrom = []string{items[0].ID}
	c.markAdmittedTodoIDs(items)
	hints := c.resolveContextQueryHints(t.Context(), ContextRequest{RunID: "run", TaskID: consumer.ID})
	if len(hints.Dependencies) != 1 || hints.Dependencies[0].TaskID != items[0].ID || !hints.Unavailable {
		t.Fatalf("unaccepted/wrong-run hint admitted, or EvidenceFrom lost: %+v", hints)
	}
	consumer.DependsOn = nil
	consumer.EvidenceFrom = nil
	hints = c.resolveContextQueryHints(t.Context(), ContextRequest{RunID: "run", TaskID: consumer.ID, DependencyIDs: []string{items[0].ID}})
	if len(hints.Dependencies) != 0 {
		t.Fatal("transient request IDs granted a dependency source")
	}
}

func TestContextQueryResolverRequiresExactWorksetPathsAndReceipt(t *testing.T) {
	_, c, producer, store := newWorksetArtifactFixture(t)
	paths := []string{"ledger/State.go", "ledger/state.go"}
	data, err := json.Marshal(WorksetManifest{SchemaVersion: worksetSchemaVersion3, Items: []WorksetItem{{Key: "one", TouchedPaths: paths}}})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), PutArtifactRequest{Content: data, Path: "manifest.json", RunID: "run-1", TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = append(producer.TypedResult.Artifacts, artifact.ArtifactRef)
	child := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "consume"}})[0]
	child.WorksetBinding = &WorksetBinding{WorksetID: "workset", ParentTaskID: "parent", ItemKey: "one", TouchedPaths: slices.Clone(paths), SourceArtifactID: artifact.ID, SourceSHA256: artifact.SHA256, SourceArtifact: artifact.ArtifactRef}
	child.WorksetReceipt = &WorksetExpansionReceipt{WorksetID: "workset", RunID: "run-1", ParentTaskID: "parent", Children: map[string]string{"one": child.ID}, ItemCount: 1, SourceArtifactID: artifact.ID, SourceSHA256: artifact.SHA256, SourceArtifact: artifact.ArtifactRef}
	c.markAdmittedTodoIDs([]*TodoItem{child})
	request := ContextRequest{RunID: "run-1", TaskID: child.ID}
	hints := c.resolveContextQueryHints(t.Context(), request)
	if !slices.Equal(hints.Paths, paths) || hints.PathSourceHash != artifact.SHA256 {
		t.Fatalf("valid receipt paths unavailable: %+v", hints)
	}
	child.WorksetBinding.TouchedPaths = []string{"unassigned/Private.go"}
	if hints := c.resolveContextQueryHints(t.Context(), request); len(hints.Paths) != 0 || !hints.Unavailable {
		t.Fatal("mutable binding claimed paths not in canonical manifest")
	}
	child.WorksetBinding.TouchedPaths = slices.Clone(paths)
	child.WorksetReceipt.RunID = "foreign"
	if hints := c.resolveContextQueryHints(t.Context(), request); len(hints.Paths) != 0 {
		t.Fatal("prior-run workset receipt granted hints")
	}
}
