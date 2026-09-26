package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// failedWorkflowCoordinator returns a gate test coordinator whose workflow
// failed in PREPARE because task 1 failed, with that task on record.
func failedWorkflowCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := gateTestCoordinator()
	c.taskTracker = NewTaskTracker()
	w, err := newRuntimeWorkflow(workflowTestSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "preparer", Desc: "produce workset", ContractID: "prepare", Phase: PhasePrepare}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "running")
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "provider failed")
	if err := w.observe(c.taskTracker.TodoList().Items()); err == nil || w.State() != PhaseFailed || w.failedTask() != item.ID {
		t.Fatalf("observe error = %v, state %s, failed task %q; want FAILED by task %s", err, w.State(), w.failedTask(), item.ID)
	}
	c.phaseWorkflow = w
	return c
}

func runGatedCoordinatorTool(t *testing.T, c *Coordinator, name, todoID string) (*scriptedCoordinatorTool, func() error) {
	t.Helper()
	inner := &scriptedCoordinatorTool{name: name, results: []scriptedToolResult{okResponse()}}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
	ctx := tools.SetToolsAllowed(context.Background(), []string{name})
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	return inner, func() error {
		_, err := gated.Run(ctx, fantasy.ToolCall{ID: "call", Name: name})
		return err
	}
}

func TestFailedWorkflowBoundsCoordinatorToolCalls(t *testing.T) {
	c := failedWorkflowCoordinator(t)
	reconcile, reconcileCall := runGatedCoordinatorTool(t, c, reconcileTaskToolName, CoordTodoID)
	for range 5 {
		if err := reconcileCall(); err != nil {
			t.Fatalf("reconcile_task after the failure: %v", err)
		}
	}
	worker, workerCall := runGatedCoordinatorTool(t, c, "view", "7")
	for range 5 {
		if err := workerCall(); err != nil {
			t.Fatalf("worker call while the workflow is failed: %v", err)
		}
	}
	view, viewCall := runGatedCoordinatorTool(t, c, "view", CoordTodoID)
	for call := 1; call <= maxCoordinatorCallsAfterWorkflowFailure; call++ {
		if err := viewCall(); err != nil {
			t.Fatalf("coordinator call %d within the bound: %v", call, err)
		}
	}
	err := viewCall()
	if !errors.Is(err, errCoordinatorToolFailure) || !strings.Contains(err.Error(), "was not executed") {
		t.Fatalf("call past the bound: error = %v, want a stream-ending refusal", err)
	}
	if view.calls != maxCoordinatorCallsAfterWorkflowFailure || reconcile.calls != 5 || worker.calls != 5 {
		t.Fatalf("executed calls: view=%d reconcile=%d worker=%d", view.calls, reconcile.calls, worker.calls)
	}
	if !c.IsWrapUp() || !c.terminalUnresolvedRun() {
		t.Fatalf("wrap-up=%v terminal unresolved=%v, want the LLM-free finalization path", c.IsWrapUp(), c.terminalUnresolvedRun())
	}
}

func TestCoordinatorCallBoundResetsWhenTheWorkflowReopens(t *testing.T) {
	c := failedWorkflowCoordinator(t)
	_, viewCall := runGatedCoordinatorTool(t, c, "view", CoordTodoID)
	for range maxCoordinatorCallsAfterWorkflowFailure {
		if err := viewCall(); err != nil {
			t.Fatal(err)
		}
	}
	if !c.phaseWorkflow.reconcileFailure(c.phaseWorkflow.failedTask()) {
		t.Fatal("workflow did not reopen")
	}
	for call := range 2 * maxCoordinatorCallsAfterWorkflowFailure {
		if err := viewCall(); err != nil {
			t.Fatalf("call %d after the workflow reopened: %v", call, err)
		}
	}
}

func TestWorkflowFailureResultNamesTheReopeningTask(t *testing.T) {
	c := failedWorkflowCoordinator(t)
	failure := errors.New("workflow prepare failed: provider failed")
	err := c.workflowFailureResult(failure)
	if !errors.Is(err, failure) || errors.Is(err, errCoordinatorFatal) || c.IsWrapUp() {
		t.Fatalf("task failure result = %v (fatal=%v wrap-up=%v), want a recoverable error", err, errors.Is(err, errCoordinatorFatal), c.IsWrapUp())
	}
	if !strings.Contains(err.Error(), reconcileTaskToolName+" on task "+c.phaseWorkflow.failedTask()) {
		t.Fatalf("task failure result = %q, want the reconcile_task instruction", err)
	}

	structural := gateTestCoordinator()
	structural.taskTracker = NewTaskTracker()
	w, buildErr := newRuntimeWorkflow(workflowTestSession(t))
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if startErr := w.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	_ = w.fail("scheduler", "scheduler", CategoryInternalError, "scheduler failed", false)
	structural.phaseWorkflow = w
	err = structural.workflowFailureResult(errors.New("workflow prepare failed: scheduler failed"))
	if !errors.Is(err, errCoordinatorFatal) || !structural.IsWrapUp() {
		t.Fatalf("structural failure result = %v (wrap-up=%v), want a fatal stop", err, structural.IsWrapUp())
	}
}
