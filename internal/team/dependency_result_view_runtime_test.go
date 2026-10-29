package team

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func dependencyViewRuntimeFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, WorkerContextInput) {
	t.Helper()
	c := newTestCoordinator(t)
	source := dependencyViewAcceptedSource(t, c)
	policy := &DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}
	consumer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "consume", Goal: "consume", ContractID: "consume", ContractHash: strings.Repeat("d", 64), ContractRevision: selectedDependencyContractRevision, DependencyResultView: policy, DependsOn: []string{source.ID}}})[0]
	if err := c.emitEvent(string(EventTaskCreated), consumer.Agent, consumer.ID, taskTransitionPayloadWithCoordinator(consumer, c)); err != nil {
		t.Fatal(err)
	}
	task := taskDefFromTodoItem(consumer)
	input := buildWorkerContextInput(c.newTaskContextRequest(task, consumer.ID, 1, ContextTriggerTaskDispatch, "worker", "worker", nil), task, c.session.Agents["worker"], "", "", "", "", nil)
	input.DependencyResults = c.dependencyResultsForTask(consumer.ID)
	input.ModelContext = ModelContextSpec{ModelID: "test", ContextWindow: 16_384, MaxOutputTokens: 256, SafetyMarginTokens: 64, Estimator: "estimated"}
	input.DisableMemory, input.DisableCanonicalMemory = true, true
	return c, source, consumer, input
}

func dependencyViewAcceptedSource(t *testing.T, c *Coordinator) *TodoItem {
	t.Helper()
	c.initEventStore()
	if c.EventStore() == nil {
		t.Fatal("canonical fixture event store unavailable")
	}
	source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "produce"}})[0]
	if err := c.emitEvent(string(EventTaskCreated), source.Agent, source.ID, taskTransitionPayloadWithCoordinator(source, c)); err != nil {
		t.Fatal(err)
	}
	result, _ := dependencyViewRenderFixture()
	result.TaskID, result.Agent = source.ID, source.Agent
	result.Artifacts, result.FilesModified, result.Verification = nil, nil, nil
	source.Status, source.OccurrenceRevision, source.TypedResult = TaskDone, 4, &result
	payload, err := json.Marshal(taskTransitionPayloadWithCoordinator(source, c))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EventStore().Append(RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCompleted), TaskID: source.ID, Attempt: 1, Actor: "worker", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestDependencyResultViewRuntimePreparesAndVerifiesDelivery(t *testing.T) {
	c, source, consumer, input := dependencyViewRuntimeFixture(t)
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compiled.Prompt, "unselected_details") || !strings.Contains(compiled.Prompt, "summary_sentinel") {
		t.Fatal("runtime delivered a different dependency presentation")
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, "worker", time.Now(), agent.MemoryLearningPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().SetContextManifest(consumer.ID, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ModelCalled || c.validateSelectedDependencyDelivery(consumer.ID, 1, manifest.RunID) == nil {
		t.Fatal("prepared request was counted as dispatched")
	}
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(t.Context(), &manifest)); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSelectedDependencyDelivery(consumer.ID, 1, manifest.RunID); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSelectedRepairContext(t.Context(), consumer, &manifest); err != nil {
		t.Fatal(err)
	}
	// The result remains canonical; its view does not become a replacement.
	if !strings.Contains(source.TypedResult.Details, "unselected_details") {
		t.Fatal("runtime modified the accepted source")
	}
	consumer = c.todoItemByID(consumer.ID)
	for i := range consumer.ContextManifests[0].Items {
		if consumer.ContextManifests[0].Items[i].ID == "dependency_results" {
			consumer.ContextManifests[0].Items[i].ContentHash = strings.Repeat("c", 64)
		}
	}
	consumer.ContextManifests[0].Fingerprint = dependencyResultViewManifestFingerprint(consumer.ContextManifests[0])
	if err := c.taskTracker.TodoList().SetContextManifest(consumer.ID, &consumer.ContextManifests[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSelectedDependencyDelivery(consumer.ID, 1, manifest.RunID); err == nil {
		t.Fatal("rewritten delivery hash was accepted")
	}
}

func TestDependencyResultViewRuntimeRejectsSourceAndPolicyDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*TodoItem, *TodoItem, *WorkerContextInput)
	}{
		{"revision", func(s, _ *TodoItem, _ *WorkerContextInput) { s.OccurrenceRevision++ }},
		{"missing result", func(s, _ *TodoItem, _ *WorkerContextInput) { s.TypedResult = nil }},
		{"missing dependency", func(_, c *TodoItem, _ *WorkerContextInput) { c.DependsOn = nil }},
		{"wrong source", func(_, _ *TodoItem, i *WorkerContextInput) { i.DependencyResults[0].TaskID = "other" }},
		{"changed policy", func(_, _ *TodoItem, i *WorkerContextInput) {
			i.TaskDef.DependencyResultView.Fields = []string{"findings"}
		}},
		{"unbound", func(_, _ *TodoItem, i *WorkerContextInput) { i.TaskDef.ContractHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, source, consumer, input := dependencyViewRuntimeFixture(t)
			tc.change(source, consumer, &input)
			if _, err := c.compileWorkerContext(t.Context(), input); err == nil {
				t.Fatal("runtime compiled a changed or missing canonical binding")
			}
		})
	}
}
