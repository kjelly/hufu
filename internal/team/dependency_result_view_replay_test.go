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
