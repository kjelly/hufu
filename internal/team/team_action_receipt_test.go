package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedOutputsProvider struct {
	executed int
	outputs  map[string]any
}

func (*fixedOutputsProvider) ProviderName() string  { return "fixed-outputs" }
func (*fixedOutputsProvider) Validate(Action) error { return nil }
func (p *fixedOutputsProvider) Execute(context.Context, Action) (interface{}, error) {
	p.executed++
	return ActionResult{Outputs: p.outputs}, nil
}

func outputSchemaCatalogCoordinator(t *testing.T, outputs map[string]any) (*Coordinator, *EventStore, *fixedOutputsProvider) {
	t.Helper()
	provider := &fixedOutputsProvider{outputs: outputs}
	session := dynamicCatalogSession(t, provider, true)
	session.ActionCatalog.Entries[0].OutputSchema = &RunInputSchema{
		Type: "object", RequiredProperties: []string{"summary"}, Properties: map[string]RunInputSchema{"summary": {Type: "string"}},
	}
	c, events := newDynamicCatalogCoordinator(t, session)
	return c, events, provider
}

func readRuntimeActionReceipt(t *testing.T, workspace string) runtimeActionReceipt {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(workspace, "runtime", "receipts", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("runtime receipts = %v, err %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt runtimeActionReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCatalogOutputSchemaMismatchFailsWithoutRetry(t *testing.T) {
	c, _, provider := outputSchemaCatalogCoordinator(t, map[string]any{"unexpected": 1})
	task := catalogActionTask(testCatalogBinding())
	_, item := addCatalogActionTodo(c, task)
	_, err := c.executeRuntimeAction(context.Background(), task, item.ID)
	var validation ActionValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), "team_action_output_invalid") {
		t.Fatalf("executeRuntimeAction error = %v, want team_action_output_invalid", err)
	}
	if provider.executed != 1 {
		t.Fatalf("provider executions = %d, want 1", provider.executed)
	}
	if got := c.taskTracker.TodoList().Items()[0]; got.Status == TaskDone || got.TypedResult != nil {
		t.Fatalf("output-invalid task = status %s result %#v", got.Status, got.TypedResult)
	}
	if (&runtimeWorkflow{actionsEnabled: true, retryState: NewRetryState()}).permitActionRetry(TaskDef{Action: task.Action, MaxRetries: 3}, err) {
		t.Fatal("an output validation failure was retryable")
	}
	receipt := readRuntimeActionReceipt(t, c.session.Workspace)
	if receipt.Status != "failure" || !strings.Contains(receipt.Error, "team_action_output_invalid") || receipt.CatalogActionID != "collect" {
		t.Fatalf("failure receipt = %#v", receipt)
	}
}

func TestCatalogActionRecordsCatalogIdentity(t *testing.T) {
	c, events, _ := outputSchemaCatalogCoordinator(t, map[string]any{"summary": "ok"})
	binding := testCatalogBinding()
	task := catalogActionTask(binding)
	_, item := addCatalogActionTodo(c, task)
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatal(err)
	}
	receipt := readRuntimeActionReceipt(t, c.session.Workspace)
	if receipt.Version != 2 || receipt.CatalogActionID != binding.ActionID || receipt.CatalogEntryHash != binding.EntryHash ||
		receipt.ArgumentsHash != binding.ArgumentsHash || receipt.CatalogInvocationID != binding.InvocationID ||
		strings.Join(receipt.ProposalIDs, ",") != "tap_a" || receipt.SideEffect != SideEffectNone {
		t.Fatalf("runtime receipt = %#v", receipt)
	}
	lifecycle := actionLifecycleEvents(t, events)
	var payload LifecycleEventPayload
	if err := json.Unmarshal(lifecycle[len(lifecycle)-1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CatalogRuntimeFields != catalogRuntimeFields(binding) {
		t.Fatalf("lifecycle catalog fields = %#v", payload.CatalogRuntimeFields)
	}
	executionReceipt := c.taskTracker.TodoList().Items()[0].ExecutionReceipt
	if executionReceipt == nil || executionReceipt.CatalogRuntimeFields != catalogRuntimeFields(binding) {
		t.Fatalf("execution receipt = %#v", executionReceipt)
	}
	cloned := cloneExecutionReceipt(executionReceipt)
	if cloned.CatalogRuntimeFields != executionReceipt.CatalogRuntimeFields {
		t.Fatal("cloneExecutionReceipt dropped the catalog fields")
	}
}

func TestNonCatalogReceiptsKeepTheirShape(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	for name, value := range map[string]any{
		"runtime receipt":   runtimeActionReceipt{Version: 2, TaskID: "1", ActionID: "a", Capability: "c", Type: "t", Status: "success", StartedAt: now, FinishedAt: now},
		"lifecycle payload": LifecycleEventPayload{Phase: "", Agent: "a"},
		"execution receipt": ExecutionReceipt{RunID: "r", TaskID: "1"},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"catalog_action_id", "catalog_entry_hash", "arguments_hash", "catalog_invocation_id", "proposal_ids", "side_effect"} {
			if strings.Contains(string(encoded), field) {
				t.Fatalf("%s of a non-catalog action mentions %s: %s", name, field, encoded)
			}
		}
	}
}
