package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestDependencyResultViewWorkerDispatchAndCompletion(t *testing.T) {
	c, source, consumer, _ := dependencyViewRuntimeFixture(t)
	before, err := json.Marshal(source.TypedResult, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	consumer.Execution.RequiresResult = true
	if _, err := c.admitTaskOccurrence(t.Context(), consumer, consumer.ID, 1); err != nil {
		t.Fatal(err)
	}
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if !strings.Contains(call.Prompt, "summary_sentinel") || strings.Contains(call.Prompt, "unselected_details") {
			return nil, fmt.Errorf("worker received an incorrect dependency presentation")
		}
		item := c.todoItemByID(consumer.ID)
		if len(item.ContextManifests) != 1 || !item.ContextManifests[0].ModelCalled {
			return nil, fmt.Errorf("actual dispatch has no delivery manifest")
		}
		response, submitErr := (&submitResultTool{coordinator: c, todoID: consumer.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"selected input consumed"}`})
		if submitErr != nil {
			return nil, submitErr
		}
		if response.IsError {
			return nil, fmt.Errorf("submit selected result: %s", response.Content)
		}
		return textAgentResult("submitted"), nil
	}})
	c.workerAgentOverride = worker
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(consumer), consumer.ID); err != nil {
		t.Fatal(err)
	}
	got := c.todoItemByID(consumer.ID)
	if worker.Calls() != 1 || got.Status != TaskDone || got.TypedResult == nil || got.ExecutionReceipt == nil {
		t.Fatalf("dispatch did not finish with a canonical result and receipt: calls=%d, task=%+v", worker.Calls(), got)
	}
	if err := c.validateSelectedDependencyDelivery(got.ID, got.TypedResult.Attempt); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(c.todoItemByID(source.ID).TypedResult, json.Deterministic(true))
	if err != nil || string(before) != string(after) {
		t.Fatalf("dispatch rewrote the canonical source: %v", err)
	}
}

func TestDependencyResultViewCancelledRequestIsNotDelivery(t *testing.T) {
	c, _, consumer, input := dependencyViewRuntimeFixture(t)
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, consumer.Agent, c.sessionTime, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(c.bindDependencyViewCall(t.Context(), &manifest))
	cancel()
	if _, err := markDependencyViewModelCall(ctx); err == nil || manifest.ModelCalled {
		t.Fatal("cancelled request was recorded as delivery")
	}
}

func TestDependencyResultViewFailedDispatchRecordIsNotDelivery(t *testing.T) {
	c, _, consumer, input := dependencyViewRuntimeFixture(t)
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, consumer.Agent, c.sessionTime, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionStore(&failOnContextManifestSessionStore{sd: &SessionData{}})
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(t.Context(), &manifest)); err == nil {
		t.Fatal("failed manifest persistence allowed dispatch")
	}
	if manifest.ModelCalled || c.validateSelectedDependencyDelivery(consumer.ID, 1, manifest.RunID) == nil {
		t.Fatal("failed dispatch record was retained as delivery proof")
	}
}

func TestDependencyResultViewAuxiliaryCallKeepsItsOwnIdentity(t *testing.T) {
	c, _, _, _ := dependencyViewRuntimeFixture(t)
	manifest := dependencyViewManifestFixture(t)
	ctx := c.bindDependencyViewCall(t.Context(), &manifest)
	auxiliary := InvocationMetadata{ParentRequestID: "auxiliary-request", ModelExecutionID: "auxiliary-execution", Purpose: "result_repair", Attempt: 1}
	ctx = withInvocationMetadata(ctx, auxiliary)
	got, err := markDependencyViewModelCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := invocationMetadataFromContext(got)
	if manifest.ModelCalled || metadata.ParentRequestID != auxiliary.ParentRequestID || metadata.ModelExecutionID != auxiliary.ModelExecutionID {
		t.Fatal("auxiliary invocation adopted or dispatched its parent request")
	}
}

func TestDependencyResultViewCheckpointCannotForgeDispatch(t *testing.T) {
	c, _, consumer, input := dependencyViewRuntimeFixture(t)
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, consumer.Agent, c.sessionTime, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.persistContextManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ModelCalled, manifest.Outcome = true, "model_call"
	manifest.Fingerprint = dependencyResultViewManifestFingerprint(manifest)
	if err := c.taskTracker.TodoList().SetContextManifest(consumer.ID, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSelectedDependencyDelivery(consumer.ID, 1, manifest.RunID); err == nil {
		t.Fatal("rewritten checkpoint manufactured a canonical dispatch")
	}
	if err := c.validateSelectedRepairContext(t.Context(), c.todoItemByID(consumer.ID), &manifest); err == nil {
		t.Fatal("result repair accepted a forged dispatch")
	}
}

func TestDependencyResultViewInvalidSourcesNeverReachWorker(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*TodoItem, *TodoItem)
	}{
		{"stale revision", func(source, _ *TodoItem) { source.OccurrenceRevision++ }},
		{"different batch", func(source, _ *TodoItem) { source.ParentID = "foreign-parent" }},
		{"structured payload", func(source, _ *TodoItem) {
			source.TypedResult.StructuredPayload = &ResultPayload{Value: []byte(`{"claim":"structured"}`)}
		}},
		{"complete evidence", func(_, consumer *TodoItem) { consumer.Execution.RequiresEvidence = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, source, consumer, _ := dependencyViewRuntimeFixture(t)
			test.change(source, consumer)
			if _, err := c.admitTaskOccurrence(t.Context(), consumer, consumer.ID, 1); err != nil {
				t.Fatal(err)
			}
			worker := scriptWorker(textTurn("must not run"))
			c.workerAgentOverride = worker
			if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(consumer), consumer.ID); err == nil || worker.Calls() != 0 {
				t.Fatalf("invalid source reached worker: calls=%d error=%v", worker.Calls(), err)
			}
		})
	}
}
