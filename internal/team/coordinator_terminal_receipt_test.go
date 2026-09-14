package team

import (
	"context"
	"testing"
)

func TestRuntimeActionPersistsTaskReceiptBeforeEvidenceManifest(t *testing.T) {
	command := []string{"/bin/sh", "-c", `set -eu
printf 'artifact' > "$HUFU_WORKSPACE/result.txt"
printf '%s' '{"outputs":{"ok":true},"artifacts":[{"path":"result.txt","kind":"provider-output"}]}'
`}
	c, _ := newCommandActionCoordinator(t, command, "run-terminal-receipt-action")
	task := TaskDef{Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "action", Phase: task.Phase, ContractID: task.ID, Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeRuntimeAction: %v", err)
	}

	got := c.todoItemByID(item.ID)
	if got == nil || got.Status != TaskDone {
		t.Fatalf("task status = %#v, want DONE", got)
	}
	if got.ExecutionReceipt == nil || len(got.ExecutionReceipts) != 1 {
		t.Fatalf("task receipts = single=%#v history=%#v, want exactly one", got.ExecutionReceipt, got.ExecutionReceipts)
	}
	receipt := got.ExecutionReceipt
	if receipt.RunID != "run-terminal-receipt-action" || receipt.TaskID != item.ID || receipt.Attempt != 1 || receipt.ProducerID == "" || receipt.ModelExecutionID == "" || receipt.TranscriptRef == "" || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		t.Fatalf("successful task receipt = %#v", receipt)
	}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatalf("buildEvidenceManifest: %v", err)
	}
	if manifest.Status != "accepted" || len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].Status != "passed" || manifest.EvidenceResults[0].Binding == nil {
		t.Fatalf("task evidence manifest = %#v, want accepted bound evidence", manifest)
	}
}

func TestRuntimeActionBindsFrozenInputsAcrossReceiptResultAndEvidence(t *testing.T) {
	command := []string{"/bin/sh", "-c", `set -eu
printf '%s' '{"outputs":{"ok":true}}'
`}
	c, _ := newCommandActionCoordinator(t, command, "run-bound-action")
	definitions := []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}}
	snapshot := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{Name: "scope", RawValue: []byte(`{"count":10}`), Source: RunInputSourceCLI}})
	c.session.RunInputDefinitions = definitions
	c.sessionData = &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID}
	tasks, err := c.materializeTaskActions([]TaskDef{{
		ID: "action", Agent: "executor", Goal: "apply", Phase: PhaseExecute,
		Action: &Action{Capability: "structured-actions", Type: "apply", Payload: `{"scope":{"count":1}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}},
	}})
	if err != nil {
		t.Fatalf("materializeTaskActions: %v", err)
	}
	task := tasks[0]
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, ContractHash: task.ContractHash,
		Action: task.Action, Agent: task.Agent, Desc: task.Goal,
		RunInputSnapshotID: task.RunInputSnapshotID, RunInputSnapshotHash: task.RunInputSnapshotHash,
		MaterializedActionPayloadHash: task.MaterializedActionPayloadHash, BoundInputs: task.BoundInputs,
	}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeRuntimeAction: %v", err)
	}

	got := c.todoItemByID(item.ID)
	if got.ExecutionReceipt == nil || got.ExecutionReceipt.RunInputSnapshotHash != snapshot.SnapshotHash || got.ExecutionReceipt.MaterializedActionPayloadHash != task.MaterializedActionPayloadHash {
		t.Fatalf("execution receipt binding = %#v", got.ExecutionReceipt)
	}
	if got.TypedResult == nil || got.TypedResult.RunInputSnapshotID != snapshot.ID || got.TypedResult.BoundInputs["scope"] == "" {
		t.Fatalf("runtime result binding = %#v", got.TypedResult)
	}
	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatalf("buildEvidenceManifest: %v", err)
	}
	binding := manifest.EvidenceResults[0].Binding
	if binding == nil || binding.RunInputSnapshotHash != snapshot.SnapshotHash || binding.BoundInputs["scope"] == "" {
		t.Fatalf("evidence binding = %#v", binding)
	}
}

func TestStructuredCoordinatorTaskPersistsTaskReceiptBeforeDone(t *testing.T) {
	workspace := t.TempDir()
	contract := ExecutionContract{Steps: []ExecutionStep{{ID: "inspect", Tool: "inspector", Effect: ExecutionEffectRead}}}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		executionRunID: "run-terminal-receipt-structured",
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "structured task", Execution: contract}})[0]
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(context.Context, StructuredStepRequest) (ExecutionStepResult, error) {
		return ExecutionStepResult{}, nil
	}))

	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "structured task", Execution: contract}, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	got := c.todoItemByID(item.ID)
	if got == nil || got.Status != TaskDone || got.ExecutionReceipt == nil || len(got.ExecutionReceipts) != 1 {
		t.Fatalf("structured terminal state = %#v, want DONE with one receipt", got)
	}
	receipt := got.ExecutionReceipt
	if receipt.RunID != c.executionRunID || receipt.TaskID != item.ID || receipt.Attempt != 1 || receipt.ProducerID == "" || receipt.ModelExecutionID == "" || receipt.TranscriptRef == "" || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		t.Fatalf("structured successful task receipt = %#v", receipt)
	}
	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatalf("buildEvidenceManifest: %v", err)
	}
	if manifest.Status != "accepted" || len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].Status != "passed" {
		t.Fatalf("structured task evidence manifest = %#v, want accepted", manifest)
	}
}
