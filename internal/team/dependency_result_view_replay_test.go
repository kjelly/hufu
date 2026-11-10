package team

import (
	"testing"
	"time"
)

func TestDependencyResultViewRetryFanoutAndReplayKeepSources(t *testing.T) {
	c, _, consumer, input := dependencyViewRuntimeFixture(t)
	var expectedHash string
	for _, test := range []struct {
		name    string
		attempt int
		trigger ContextTrigger
		leaf    bool
	}{
		{"primary", 1, ContextTriggerTaskDispatch, false},
		{"retry", 2, ContextTriggerRetry, false},
		{"extra-model", 2, ContextTriggerRetry, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := c
			if test.leaf {
				runtime = newTestCoordinator(t)
				runtime.modelFanout = &modelFanoutBinding{parent: c, taskID: consumer.ID}
				runtime.taskTracker.TodoList().AddReserved([]*TodoItem{c.todoItemByID(consumer.ID)})
			}
			requestInput := input
			requestInput.Request.Attempt, requestInput.Request.Trigger = test.attempt, test.trigger
			requestInput.Request.Purpose = contextPurposeForTrigger(test.trigger)
			if test.trigger == ContextTriggerRetry {
				requestInput.Request.Failure = &ContextFailure{ErrorClass: "timeout"}
			}
			requestInput.Request.ModelExecutionID = test.name
			requestInput.Request.AssignRequestID()
			compiled, err := runtime.compileWorkerContext(t.Context(), requestInput)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := BuildContextInjectionManifest(requestInput.Request, compiled, nil, consumer.Agent, time.Now(), c.session.Config.MemoryLearning)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := markDependencyViewModelCall(runtime.bindDependencyViewCall(t.Context(), &manifest)); err != nil {
				t.Fatal(err)
			}
			for _, item := range manifest.Items {
				if item.DependencyResultView != nil {
					if expectedHash == "" {
						expectedHash = item.ContentHash
					}
					if expectedHash != item.ContentHash {
						t.Fatal("retry or fanout changed the frozen view")
					}
				}
			}
		})
	}
	events, err := c.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
	if err := c.validateSelectedDependencyDelivery(consumer.ID, 1, input.Request.RunID); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSelectedDependencyDelivery(consumer.ID, 2, input.Request.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.compileWorkerContext(t.Context(), input); err != nil {
		t.Fatalf("replayed canonical context changed: %v", err)
	}
}

func TestDependencyResultViewCompletionOrderKeepsPresentation(t *testing.T) {
	var expected string
	for _, test := range []struct {
		name  string
		order []int
	}{{"forward", []int{0, 1}}, {"reverse", []int{1, 0}}} {
		t.Run(test.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.initEventStore()
			sources := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "first source"}, {Agent: "worker", Desc: "second source"}})
			for _, source := range sources {
				if err := c.emitEvent(string(EventTaskCreated), source.Agent, source.ID, taskTransitionPayloadWithCoordinator(source, c)); err != nil {
					t.Fatal(err)
				}
				result, _ := dependencyViewRenderFixture()
				result.TaskID, result.Agent, result.Summary = source.ID, source.Agent, "source "+source.ID
				result.Artifacts, result.FilesModified, result.Verification = nil, nil, nil
				source.Status, source.OccurrenceRevision, source.TypedResult = TaskDone, 4, &result
			}
			for _, index := range test.order {
				source := sources[index]
				if err := c.emitEvent(string(EventTaskCompleted), source.Agent, source.ID, taskTransitionPayloadWithCoordinator(source, c)); err != nil {
					t.Fatal(err)
				}
			}
			consumer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "consume", Goal: "consume", ContractID: "consume", ContractHash: dependencyViewDigest([]byte("consume")), ContractRevision: selectedDependencyContractRevision,
				DependencyResultView: &DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, DependsOn: []string{sources[1].ID, sources[0].ID}}})[0]
			task := taskDefFromTodoItem(consumer)
			input := buildWorkerContextInput(c.newTaskContextRequest(task, consumer.ID, 1, ContextTriggerTaskDispatch, "worker", "worker", nil), task, c.session.Agents["worker"], "", "", "", "", nil)
			input.DependencyResults = c.dependencyResultsForTask(consumer.ID)
			input.ModelContext = ModelContextSpec{ModelID: "test", ContextWindow: 16_384, MaxOutputTokens: 256, SafetyMarginTokens: 64}
			input.DisableMemory, input.DisableCanonicalMemory = true, true
			compiled, err := c.compileWorkerContext(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			var actual string
			for _, item := range compiled.IncludedItems {
				if item.DependencyResultView == nil {
					continue
				}
				if item.DependencyResultView.Sources[0].TaskID != sources[0].ID || item.DependencyResultView.Sources[1].TaskID != sources[1].ID {
					t.Fatal("completion order changed canonical source order")
				}
				actual = dependencyViewDigest([]byte(item.Content))
			}
			if actual == "" {
				t.Fatal("selected context item was not delivered")
			}
			if expected == "" {
				expected = actual
			} else if actual != expected {
				t.Fatal("upstream completion order changed the selected presentation digest")
			}
		})
	}
}
