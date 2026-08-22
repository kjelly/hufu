package team

import (
	"context"
	"os"
	"strings"
	"testing"
)

func structuredPublicationFixture(t *testing.T) (*Coordinator, *TodoItem, TaskDef, *FileArtifactStore) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	contract := ExecutionContract{Steps: []ExecutionStep{{
		ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce,
		Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact}},
	}}}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		executionRunID: "run-structured-publication",
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
		taskResults:    make(map[string]*TaskResult),
		taskAttempts:   make(map[string]int),
		stepReceipts:   NewExecutionStepReceiptRegistry(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "publish structured artifact", Execution: contract}})[0]
	return c, item, TaskDef{Agent: item.Agent, Goal: item.Desc, Execution: contract}, store
}

func TestStructuredPublicationRejectsMissingProvenanceBeforeCanonicalResult(t *testing.T) {
	c, item, task, store := structuredPublicationFixture(t)
	ref, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("draft"), Path: "draft.txt", RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
	if err != nil {
		t.Fatal(err)
	}
	ref.RunID = ""
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": ref.ArtifactRef}}, nil
	}))

	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("missing provenance error = %v", err)
	}
	if c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
		t.Fatalf("canonical result persisted after publication rejection: item=%#v result=%#v", item, c.GetTaskResult(item.ID))
	}
}

func TestStructuredPublicationRejectsStaleOccurrenceAndForgedContentClaims(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ArtifactRef)
		want   string
	}{
		{name: "wrong run", mutate: func(ref *ArtifactRef) { ref.RunID = "stale-run" }, want: "provenance"},
		{name: "wrong task", mutate: func(ref *ArtifactRef) { ref.TaskID = "stale-task" }, want: "provenance"},
		{name: "wrong attempt", mutate: func(ref *ArtifactRef) { ref.Attempt = 2 }, want: "provenance"},
		{name: "wrong agent", mutate: func(ref *ArtifactRef) { ref.Agent = "other" }, want: "provenance"},
		{name: "forged id", mutate: func(ref *ArtifactRef) { ref.ID = "forged-id" }},
		{name: "forged digest", mutate: func(ref *ArtifactRef) { ref.SHA256 = strings.Repeat("0", 64) }},
		{name: "forged size", mutate: func(ref *ArtifactRef) { ref.Bytes++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, item, task, store := structuredPublicationFixture(t)
			ref, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("draft"), Path: "draft.txt", RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
			if err != nil {
				t.Fatal(err)
			}
			supplied := ref.ArtifactRef
			tc.mutate(&supplied)
			c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
				return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": supplied}}, nil
			}))
			if _, err := c.executeTask(context.Background(), task, item.ID); err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("rejected artifact error = %v, want %q", err, tc.want)
			}
			if c.GetTaskResult(item.ID) != nil {
				t.Fatalf("canonical result persisted after %s rejection", tc.name)
			}
		})
	}
}

func TestStructuredPublicationRejectsOmittedSizeClaimsForExistingNonEmptyArtifact(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ArtifactRef)
	}{
		{name: "bytes", mutate: func(ref *ArtifactRef) { ref.Bytes = 0 }},
		{name: "byte size", mutate: func(ref *ArtifactRef) { ref.ByteSize = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, item, task, store := structuredPublicationFixture(t)
			ref, err := store.Put(context.Background(), PutArtifactRequest{
				Content: []byte("non-empty"), Path: "draft.txt", RunID: c.executionRunID,
				TaskID: item.ID, Attempt: 1, Agent: item.Agent,
			})
			if err != nil {
				t.Fatal(err)
			}
			supplied := ref.ArtifactRef
			tc.mutate(&supplied)
			c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
				return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": supplied}}, nil
			}))

			if _, err := c.executeTask(context.Background(), task, item.ID); err == nil || !strings.Contains(err.Error(), "requires both") {
				t.Fatalf("size-claim error = %v", err)
			}
			if result := c.GetTaskResult(item.ID); result != nil || item.TypedResult != nil {
				t.Fatalf("canonical result persisted after %s size-claim rejection: result=%#v item=%#v", tc.name, result, item.TypedResult)
			}
		})
	}
}

func TestStructuredPublicationAcceptsExactZeroByteClaims(t *testing.T) {
	c, item, task, store := structuredPublicationFixture(t)
	ref, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte{}, Path: "empty.txt", RunID: c.executionRunID,
		TaskID: item.ID, Attempt: 1, Agent: item.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": ref.ArtifactRef}}, nil
	}))

	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("zero-byte artifact publication failed: %v", err)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil || result.Outputs["draft"].Artifact == nil {
		t.Fatalf("canonical zero-byte structured output = %#v", result)
	}
	artifact := result.Outputs["draft"].Artifact
	if artifact.Bytes != 0 || artifact.ByteSize != 0 {
		t.Fatalf("published zero-byte artifact = %#v", artifact)
	}
}

func TestStructuredPublicationMaterializesSourceAndAttestsCurrentOccurrence(t *testing.T) {
	c, item, task, _ := structuredPublicationFixture(t)
	path := "generated/draft.txt"
	if err := os.MkdirAll(c.session.Workspace+"/generated", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.session.Workspace+"/"+path, []byte("source content"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {
			Path: path, RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent,
		}}}, nil
	}))

	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("source artifact publication failed: %v", err)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil || result.Outputs["draft"].Artifact == nil {
		t.Fatalf("canonical structured output = %#v", result)
	}
	artifact := result.Outputs["draft"].Artifact
	if artifact.ID == "" || artifact.SHA256 == "" || artifact.Bytes != int64(len("source content")) || artifact.ByteSize != artifact.Bytes || artifact.RunID != c.executionRunID || artifact.TaskID != item.ID || artifact.Attempt != 1 || artifact.Agent != item.Agent {
		t.Fatalf("published artifact = %#v", artifact)
	}
}

func TestStructuredPublicationRejectsCASTampering(t *testing.T) {
	c, item, task, store := structuredPublicationFixture(t)
	ref, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("draft"), Path: "draft.txt", RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
	if err != nil {
		t.Fatal(err)
	}
	dataPath := c.session.Workspace + "/logs/artifacts/data/" + ref.ID
	if err := os.WriteFile(dataPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref.Path = ""
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": ref.ArtifactRef}}, nil
	}))

	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil || !strings.Contains(err.Error(), "hash or size mismatch") {
		t.Fatalf("CAS tampering error = %v", err)
	}
	if c.GetTaskResult(item.ID) != nil {
		t.Fatal("canonical result persisted after CAS tampering")
	}
}
