package team

import (
	"context"
	"errors"
)

// Keep an explicit graceful request distinct from budget/policy stops and
// wrapUpFromBlockedTask. Recovering a worker must not clear this request.
const wrapUpFromGracefulRequest int32 = 3

// workflowWrapUpReady is checked only at coordinator boundaries. Workers keep
// their normal tool, receipt and verification paths until they are terminal.
func (c *Coordinator) workflowWrapUpReady() bool {
	if c == nil || c.wrapUp.Load() != wrapUpFromGracefulRequest || c.acceptanceRecovery.Load() || c.noProgressStopPending() {
		return false
	}
	w := c.phaseWorkflow
	if w == nil || !w.Enabled() || !IsValidTransition(w.State(), PhaseFailed) || w.verificationSatisfied() {
		return false
	}
	if c.taskTracker != nil && len(pendingTodoItems(c.taskTracker.TodoList().Items())) > 0 {
		return false
	}
	return c.terminalSessionMgr == nil || c.terminalSessionMgr.RequireNoLeaks("") == nil
}

// finalizeWorkflowWrapUp closes an unfinished phase after the active work has
// drained. It uses the canonical cancellation and terminal writers, never a
// model-authored report or an acceptance command for an incomplete workflow.
func (c *Coordinator) finalizeWorkflowWrapUp(ctx context.Context) (string, bool) {
	if errors.Is(context.Cause(ctx), ErrInvocationStalled) || !c.workflowWrapUpReady() {
		return "", false
	}
	const reason = "graceful wrap-up requested before workflow verification completed"
	_ = c.phaseWorkflow.cancel("coordinator", "operator", reason)
	c.saveCheckpoint()
	if checkpoint := c.ContinuationCheckpoint(); checkpoint != nil && (checkpoint.Status == "pending" || checkpoint.Status == "resumed") {
		c.saveContinuationCheckpoint(checkpoint.TurnCount, checkpoint.MaxTurns, reason, "aborted")
	}
	var items []*TodoItem
	if c.taskTracker != nil {
		items = c.taskTracker.TodoList().Items()
	}
	summary := c.summaryFromTodos(errors.New(reason))
	if summary == "" {
		summary = reason
	}
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: UnresolvedTaskReferences(items),
		Cancelled:       true,
		Response:        summary,
		Reason:          reason,
		Stats:           SummarizeRunStats(items),
		Metrics:         c.Metrics(),
		GoalMode:        c.GoalMode(),
	})
	// No acceptance command ran at this cancellation boundary.
	evaluated.Acceptance = nil
	result := c.requestTerminalResult(ctx, TerminalEntrySignal, "cancelled", false, &evaluated, nil)
	c.finishCalled.Store(true)
	c.coordinatorPolicyRepairPending.Store(false)
	return result.Response, true
}
