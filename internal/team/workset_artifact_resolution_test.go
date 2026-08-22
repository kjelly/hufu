package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	runtimeTools "github.com/kjelly/hufu/internal/tools"
)

func TestStructuredFanOutResolvesBytesOnlyInputToCanonicalOccurrence(t *testing.T) {
	workspace, c, producer, store := newWorksetArtifactFixture(t)
	input, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte("diff contents"), Path: "diff.patch", Kind: "diff", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep",
	})
	if err != nil {
		t.Fatal(err)
	}
	bytesOnly := ArtifactRef{ID: input.ID, SHA256: input.SHA256, Bytes: input.Bytes}
	expanded := expandWorksetWithInput(t, workspace, c, producer, store, bytesOnly, true)
	resolved := expanded[0].WorksetBinding.Inputs[0]
	if resolved.ID != input.ID || resolved.SHA256 != input.SHA256 || resolved.Bytes != input.Bytes || resolved.ByteSize != input.ByteSize {
		t.Fatalf("resolved input = %#v, want canonical metadata %#v", resolved, input)
	}
	if resolved.RunID != "run-1" || resolved.TaskID != producer.ID || resolved.Attempt != producer.TypedResult.Attempt || resolved.Agent != producer.TypedResult.Agent {
		t.Fatalf("resolved input provenance = %#v", resolved)
	}
}

func TestStructuredFanOutRejectsConflictingInputSizeClaims(t *testing.T) {
	for name, mutate := range map[string]func(*ArtifactRef){
		"bytes":     func(ref *ArtifactRef) { ref.Bytes++ },
		"byte_size": func(ref *ArtifactRef) { ref.ByteSize++ },
	} {
		t.Run(name, func(t *testing.T) {
			workspace, c, producer, store := newWorksetArtifactFixture(t)
			input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("content"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
			if err != nil {
				t.Fatal(err)
			}
			mutate(&input.ArtifactRef)
			if _, err := expandWorksetWithInputResult(workspace, c, producer, store, input.ArtifactRef, true); err == nil || !strings.Contains(err.Error(), "conflicts with immutable metadata") {
				t.Fatalf("conflicting size claim error = %v", err)
			}
		})
	}
}

func TestStructuredFanOutRejectsWrongDigestAndTamperedData(t *testing.T) {
	t.Run("wrong digest", func(t *testing.T) {
		workspace, c, producer, store := newWorksetArtifactFixture(t)
		input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("content"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		input.SHA256 = strings.Repeat("0", 64)
		if _, err := expandWorksetWithInputResult(workspace, c, producer, store, input.ArtifactRef, true); err == nil || !strings.Contains(err.Error(), "digest conflicts") {
			t.Fatalf("wrong digest error = %v", err)
		}
	})

	t.Run("tampered data", func(t *testing.T) {
		workspace, c, producer, store := newWorksetArtifactFixture(t)
		input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("content"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		dataPath := filepath.Join(workspace, logsDir, "artifacts", "data", input.ID)
		if err := os.WriteFile(dataPath, []byte("tampered"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := expandWorksetWithInputResult(workspace, c, producer, store, input.ArtifactRef, true); err == nil || !strings.Contains(err.Error(), "hash or size mismatch") {
			t.Fatalf("tampered data error = %v", err)
		}
	})
}

func TestStructuredFanOutResolvesZeroByteInput(t *testing.T) {
	workspace, c, producer, store := newWorksetArtifactFixture(t)
	input, err := store.Put(context.Background(), PutArtifactRequest{Content: nil, Path: "empty", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
	if err != nil {
		t.Fatal(err)
	}
	bytesOnly := ArtifactRef{ID: input.ID, SHA256: input.SHA256}
	expanded := expandWorksetWithInput(t, workspace, c, producer, store, bytesOnly, true)
	resolved := expanded[0].WorksetBinding.Inputs[0]
	if resolved.Bytes != 0 || resolved.ByteSize != 0 || resolved.SHA256 != input.SHA256 {
		t.Fatalf("zero-byte resolved input = %#v", resolved)
	}
}

func TestStructuredFanOutRejectsClearedSourceSizeClaims(t *testing.T) {
	_, c, producer, store := newWorksetArtifactFixture(t)
	manifestBytes := []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}}]}`)
	manifest, err := store.Put(context.Background(), PutArtifactRequest{
		Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest",
		RunID: "run-1", TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleared := manifest.ArtifactRef
	cleared.Bytes = 0
	cleared.ByteSize = 0
	producer.TypedResult = &TaskResult{TaskID: producer.ID, Agent: producer.Agent, Attempt: 4, Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{cleared}}
	c.taskResults[producer.ID] = producer.TypedResult
	if _, err := c.expandFanOutTasks([]TaskDef{{FanOut: &FanOutSpec{
		SourceArtifact: FactRef{TaskID: "producer", Artifact: manifest.ID}, GoalTemplate: "process {name}",
	}}}); err == nil || !strings.Contains(err.Error(), "byte count claims") {
		t.Fatalf("cleared source size claims error = %v", err)
	}
}

func TestStructuredProducerOutputFeedsWorksetFanOutAndAcceptance(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "manifest", Kind: ExecutionOutputArtifact}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"manifest"}},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"manifest"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace}, taskTracker: tracker, executionRunID: "run-structured-workset",
		taskResults: make(map[string]*TaskResult), reportStatus: func(StatusEvent) {},
	}
	producerItem := tracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "producer", Agent: "producer", Desc: "produce manifest", Execution: contract},
	})[0]
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		if request.Step.ID != "produce" {
			return ExecutionStepResult{}, nil
		}
		manifest := []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}}]}`)
		ref, putErr := store.Put(context.Background(), PutArtifactRequest{
			Content: manifest, Path: "manifest.json", Kind: "workset_manifest", RunID: c.executionRunID,
			TaskID: request.TaskID, Attempt: request.Attempt, Agent: producerItem.Agent,
		})
		if putErr != nil {
			return ExecutionStepResult{}, putErr
		}
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"manifest": ref.ArtifactRef}}, nil
	}))
	producerTask := TaskDef{ID: "producer", Agent: producerItem.Agent, Goal: "produce manifest", Execution: contract}
	if _, err := c.executeTask(context.Background(), producerTask, producerItem.ID); err != nil {
		t.Fatalf("execute structured producer: %v", err)
	}
	producerResult := c.GetTaskResult(producerItem.ID)
	if producerResult == nil || len(producerResult.Artifacts) != 0 || producerResult.Outputs["manifest"].Artifact == nil {
		t.Fatalf("structured producer result = %#v, want artifact only in Outputs", producerResult)
	}

	reviewTask := TaskDef{ID: "review", Agent: "reviewer", Goal: "review manifest", FanOut: &FanOutSpec{
		SourceArtifact: FactRef{TaskID: "producer", Artifact: "manifest"}, GoalTemplate: "review {name}",
	}}
	expanded, err := c.expandFanOutTasks([]TaskDef{reviewTask})
	if err != nil || len(expanded) != 1 {
		t.Fatalf("expand structured workset = %#v, err=%v", expanded, err)
	}
	ids := tracker.TodoList().ReserveIDs(len(expanded))
	receipts, err := buildWorksetReceipts(expanded, ids, c.executionRunID)
	if err != nil {
		t.Fatalf("build workset receipt: %v", err)
	}
	children := make([]*TodoItem, 0, len(expanded))
	for index, task := range expanded {
		children = append(children, todoItemFromSpec(TodoSpec{
			PlanTaskID: task.ID, Agent: task.Agent, Desc: task.Goal, WorksetBinding: task.WorksetBinding,
		}, ids[index]))
	}
	tracker.TodoList().AddReserved(children)
	children[0].WorksetReceipt = receipts[expanded[0].WorksetBinding.WorksetID]
	children[0].TypedResult = &TaskResult{TaskID: children[0].ID, Agent: children[0].Agent, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "reviewed"}
	children[0].VerifyResult = &VerificationResult{ExitCode: 0}
	tracker.TodoList().UpdateStatus(children[0].ID, TaskDone, "reviewed")
	c.acceptanceSpec = &AcceptanceSpec{Verifications: []VerificationSpec{{
		Type: VerifyWorksetComplete, WorksetSourceTask: reviewTask.ID, WorksetRequireTerminal: true, WorksetRequireVerified: true,
		WorksetAcceptedStatuses: []string{TaskResultStatusSuccess},
	}}}
	accepted, err := c.runAcceptance(context.Background())
	if err != nil || accepted == nil || !accepted.IsPassed() {
		t.Fatalf("structured workset acceptance = %#v, err=%v", accepted, err)
	}
}

func TestStructuredFanOutRejectsInputUndeclaredBySelectedProducer(t *testing.T) {
	workspace, c, producer, store := newWorksetArtifactFixture(t)
	input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("content"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expandWorksetWithInputResult(workspace, c, producer, store, input.ArtifactRef, false); err == nil || !strings.Contains(err.Error(), "was not declared by producer task") {
		t.Fatalf("undeclared input error = %v", err)
	}
}

func TestGeneratedWorksetChildCanViewAssignedInput(t *testing.T) {
	workspace, c, producer, store := newWorksetArtifactFixture(t)
	input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("assigned diff\n"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
	if err != nil {
		t.Fatal(err)
	}
	expanded := expandWorksetWithInput(t, workspace, c, producer, store, ArtifactRef{ID: input.ID, SHA256: input.SHA256, Bytes: input.Bytes}, true)
	child := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "generated-child", Agent: "reviewer"}})[0]
	child.WorksetBinding = cloneWorksetBinding(expanded[0].WorksetBinding)
	receipts, err := buildWorksetReceipts(expanded, []string{child.ID}, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	child.WorksetReceipt = cloneWorksetReceipt(receipts[child.WorksetBinding.WorksetID])
	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	response, err := view.Run(ctx, fantasy.ToolCall{Input: fmt.Sprintf(`{"artifact_ref":%q}`, input.ID)})
	if err != nil || response.IsError || !strings.Contains(response.Content, "assigned diff") {
		t.Fatalf("generated child view response=%#v err=%v", response, err)
	}
}

func TestStructuredFanOutRejectsStaleOccurrencesButAcceptsRepeatedContent(t *testing.T) {
	t.Run("stale manifest occurrence", func(t *testing.T) {
		_, c, producer, store := newWorksetArtifactFixture(t)
		manifestBytes := []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}}]}`)
		stale, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest", RunID: "old-run", TaskID: producer.ID, Attempt: 1, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		c.taskResults[producer.ID] = &TaskResult{TaskID: producer.ID, Agent: "reviewprep", Attempt: 4, Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{stale.ArtifactRef}}
		if _, err := c.expandFanOutTasks([]TaskDef{{FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "producer", Artifact: stale.ID}, GoalTemplate: "process {name}"}}}); err == nil || !strings.Contains(err.Error(), "belongs to run") {
			t.Fatalf("stale manifest occurrence error = %v", err)
		}
	})

	t.Run("repeated identical manifest and input content", func(t *testing.T) {
		_, c, producer, store := newWorksetArtifactFixture(t)
		oldInput, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("same input"), Path: "input", RunID: "old-run", TaskID: producer.ID, Attempt: 1, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		currentInput, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("same input"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		if oldInput.ID != currentInput.ID {
			t.Fatalf("same content IDs differ: old=%q current=%q", oldInput.ID, currentInput.ID)
		}
		manifestBytes, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{currentInput.ArtifactRef}}}})
		if err != nil {
			t.Fatal(err)
		}
		oldManifest, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest", RunID: "old-run", TaskID: producer.ID, Attempt: 1, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		currentManifest, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		if oldManifest.ID != currentManifest.ID {
			t.Fatalf("same manifest IDs differ: old=%q current=%q", oldManifest.ID, currentManifest.ID)
		}
		c.taskResults[producer.ID] = &TaskResult{TaskID: producer.ID, Agent: "reviewprep", Attempt: 4, Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{currentManifest.ArtifactRef, currentInput.ArtifactRef}}
		expanded, err := c.expandFanOutTasks([]TaskDef{{FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "producer", Artifact: currentManifest.ID}, GoalTemplate: "process {name}"}}})
		if err != nil || len(expanded) != 1 {
			t.Fatalf("repeated identical content expansion = %#v err=%v", expanded, err)
		}
		resolved := expanded[0].WorksetBinding.Inputs[0]
		if resolved.RunID != "run-1" || resolved.TaskID != producer.ID || resolved.Attempt != 4 || resolved.Agent != "reviewprep" {
			t.Fatalf("current input occurrence = %#v", resolved)
		}
	})

	t.Run("stale input occurrence", func(t *testing.T) {
		_, c, producer, store := newWorksetArtifactFixture(t)
		staleInput, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("same input"), Path: "input", RunID: "old-run", TaskID: producer.ID, Attempt: 1, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		currentInput, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("same input"), Path: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		manifestBytes, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{staleInput.ArtifactRef}}}})
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest", RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep"})
		if err != nil {
			t.Fatal(err)
		}
		c.taskResults[producer.ID] = &TaskResult{TaskID: producer.ID, Agent: "reviewprep", Attempt: 4, Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{manifest.ArtifactRef, currentInput.ArtifactRef}}
		if _, err := c.expandFanOutTasks([]TaskDef{{FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "producer", Artifact: manifest.ID}, GoalTemplate: "process {name}"}}}); err == nil || !strings.Contains(err.Error(), "belongs to run") {
			t.Fatalf("stale input occurrence error = %v", err)
		}
	})
}

func TestStructuredFanOutRejectsMissingCurrentProducerOccurrence(t *testing.T) {
	for _, name := range []string{"sparse manifest", "repeated content id without current evidence"} {
		t.Run(name, func(t *testing.T) {
			_, c, producer, store := newWorksetArtifactFixture(t)
			manifestBytes := []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}}]}`)
			current, err := store.Put(context.Background(), PutArtifactRequest{
				Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest",
				RunID: "run-1", TaskID: producer.ID, Attempt: 4, Agent: "reviewprep",
			})
			if err != nil {
				t.Fatal(err)
			}
			if name == "repeated content id without current evidence" {
				old, putErr := store.Put(context.Background(), PutArtifactRequest{
					Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest",
					RunID: "old-run", TaskID: producer.ID, Attempt: 1, Agent: "reviewprep",
				})
				if putErr != nil || old.ID != current.ID {
					t.Fatalf("repeated content setup old=%#v current=%#v err=%v", old, current, putErr)
				}
			}
			sparse := ArtifactRef{ID: current.ID, SHA256: current.SHA256, Bytes: current.Bytes, ByteSize: current.ByteSize}
			result := &TaskResult{TaskID: producer.ID, Attempt: 4, Agent: "reviewprep", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{sparse}}
			c.taskResults[producer.ID] = result
			producer.TypedResult = result
			_, err = c.expandFanOutTasks([]TaskDef{{ID: "consumer", Agent: "reviewer", FanOut: &FanOutSpec{
				SourceArtifact: FactRef{TaskID: "producer", Artifact: current.ID}, GoalTemplate: "process {name}",
			}}})
			if err == nil || !strings.Contains(err.Error(), "invalid current producer occurrence") {
				t.Fatalf("sparse producer occurrence error = %v", err)
			}
		})
	}
}

func newWorksetArtifactFixture(t *testing.T) (string, *Coordinator, *TodoItem, *FileArtifactStore) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "producer", Agent: "reviewprep", Desc: "produce workset"}})[0]
	producer.Status = TaskDone
	result := &TaskResult{TaskID: producer.ID, Agent: "reviewprep", Attempt: 4, Status: TaskResultStatusSuccess, Source: "runtime"}
	producer.TypedResult = result
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1", taskTracker: tracker, taskResults: map[string]*TaskResult{producer.ID: result}}
	return workspace, c, producer, store
}

func expandWorksetWithInput(t *testing.T, workspace string, c *Coordinator, producer *TodoItem, store *FileArtifactStore, input ArtifactRef, declared bool) []TaskDef {
	t.Helper()
	expanded, err := expandWorksetWithInputResult(workspace, c, producer, store, input, declared)
	if err != nil {
		t.Fatal(err)
	}
	return expanded
}

func expandWorksetWithInputResult(_ string, c *Coordinator, producer *TodoItem, store *FileArtifactStore, input ArtifactRef, declared bool) ([]TaskDef, error) {
	manifestBytes, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{input}}}})
	if err != nil {
		return nil, err
	}
	manifest, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifest.json", Kind: "workset_manifest", RunID: "run-1", TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent})
	if err != nil {
		return nil, err
	}
	artifacts := []ArtifactRef{manifest.ArtifactRef}
	if declared {
		declaredInput, resolveErr := store.Resolve(context.Background(), ArtifactRef{ID: input.ID})
		if resolveErr != nil {
			return nil, resolveErr
		}
		artifacts = append(artifacts, declaredInput)
	}
	currentResult := &TaskResult{TaskID: producer.ID, Agent: producer.TypedResult.Agent, Attempt: producer.TypedResult.Attempt, Status: TaskResultStatusSuccess, Source: "runtime", Artifacts: artifacts}
	c.taskResults[producer.ID] = currentResult
	producer.TypedResult = currentResult
	return c.expandFanOutTasks([]TaskDef{{ID: "consumer", Agent: "reviewer", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "producer", Artifact: manifest.ID}, GoalTemplate: "process {name}"}}})
}
