package team

import (
	"context"
	"errors"
	"strings"
)

// RunTask executes one explicitly supplied root task without an LLM
// coordinator planning turn. It owns the same public invocation lifecycle as
// Run and RunDirectAgent and dispatches through ExecuteTasks, preserving task
// admission, authorization, receipts, verification, and terminal persistence.
// Whole-run acceptance semantics are unchanged: a verified task without a
// configured acceptance gate still yields an unverified RunResult and error.
// Use a fresh coordinator/session; recovery of an interrupted invocation must
// inspect its existing evidence rather than call this entry point again.
func (c *Coordinator) RunTask(ctx context.Context, task TaskDef) (output string, err error) {
	if c == nil || c.session == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return "", errors.New("single task requires a coordinator and task tracker")
	}
	if strings.TrimSpace(task.Agent) == "" || strings.TrimSpace(task.Goal) == "" || task.Sidecar ||
		task.FanOut != nil || len(task.DependsOn) != 0 || len(task.DependsOnContracts) != 0 || len(task.OrderAfter) != 0 ||
		task.Pipeline || task.OnFailure != nil || task.OnFailureContract != "" {
		return "", errors.New("single task requires a worker and goal without fan-out or sibling edges")
	}
	if len(c.taskTracker.TodoList().Items()) != 0 {
		return "", errors.New("single task requires a fresh task scope")
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return "", errors.New("single task invocation is disabled for runtime workflows")
	}
	task = cloneTaskDef(task)
	ctx = withDirectAgentInvocation(ctx)
	ctx, end, err := c.beginPublicInvocationExecutionRun(ctx)
	if err != nil {
		return "", err
	}
	defer end()
	defer func() {
		if err != nil && c.LastRunResult() == nil {
			c.finalizePublicInvocationFailure(err)
		}
	}()
	// The earlier check is a cheap preflight. Recheck while holding the public
	// invocation lease: another invocation may have populated the task scope
	// before this call acquired ownership.
	if len(c.taskTracker.TodoList().Items()) != 0 {
		return "", errors.New("single task requires a fresh task scope")
	}
	if err := c.checkRunAdmission(); err != nil {
		return "", err
	}
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		return "", err
	}
	if err := c.reconcileAttemptWorlds(ctx); err != nil {
		return "", err
	}
	if err := c.startSemanticRunInputBoundary(ctx); err != nil {
		return "", err
	}
	if err := c.resolveRunInputsForInvocation(ctx, task.Goal); err != nil {
		return "", err
	}
	ctx = withoutCoordinatorRequestPreflight(ctx)
	output, runErr := c.ExecuteTasks(ctx, []TaskDef{task})
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 {
		if runErr == nil {
			runErr = errors.New("single task produced no durable occurrence")
		}
		return output, runErr
	}
	result := c.finalizeDirectRun(ctx, items[0].ID, runErr == nil && c.allTasksCompletedSuccessfully(), output)
	if result == nil {
		return output, errors.Join(runErr, errors.New("single task has no terminal result"))
	}
	return output, errors.Join(runErr, runCompletionError(result))
}
