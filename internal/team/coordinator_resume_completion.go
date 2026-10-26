package team

import (
	"context"
	"fmt"
)

func (c *Coordinator) resumeInvocationTasks(ctx context.Context, prompt string, newWorkflowCycle bool) (string, bool, error) {
	resumed, resumeErr := c.ResumeInterruptedTasks(ctx)
	if _, explicit := ctx.Value(resultOnlyRepairKey{}).(string); explicit && resumeErr != nil {
		return "", true, c.finalizePublicInvocationFailureError(resumeErr)
	}
	if _, explicit := ctx.Value(resultOnlyRepairKey{}).(string); explicit && len(UnresolvedTaskReferences(c.taskTracker.TodoList().Items())) > 0 {
		return "", true, c.finalizePublicInvocationFailureError(fmt.Errorf("result-only recovery left other unresolved tasks; inspect their state before ordinary resume"))
	}
	if resumeErr != nil {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s), with errors: %v", resumed, resumeErr)))
	} else if resumed > 0 {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s) from checkpoint", resumed)))
	}
	if response, stopped := c.finalizeOperatorBlockedResume(ctx, prompt, newWorkflowCycle); stopped {
		return response, true, runCompletionError(c.LastRunResult())
	}
	c.continuationResume = nil
	return "", false, nil
}

// finalizeOperatorBlockedResume closes failed recovery before coordinator
// generation when every remaining task explicitly requires operator action.
// Retryable work and tasks eligible for model replanning retain their normal
// continuation path. Completed task results and original receipts stay intact.
func (c *Coordinator) finalizeOperatorBlockedResume(ctx context.Context, prompt string, newWorkflowCycle bool) (string, bool) {
	if ctx.Err() != nil || c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return "", false
	}
	unresolved := UnresolvedTaskReferences(c.taskTracker.TodoList().Items())
	if len(unresolved) == 0 {
		return "", false
	}
	for _, item := range unresolved {
		if item.Status != string(TaskBlocked) || item.RetryDisposition != NeedsHuman {
			return "", false
		}
	}
	c.continuationResume = nil
	if !newWorkflowCycle {
		c.addSessionUserMessage(prompt)
	}
	response := c.finalizeTerminalUnresolvedRun()
	c.addSessionAssistantMessage(response)
	c.persistSessionRounds()
	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withMessage("resume requires operator review").withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
	c.SetCurrentStage("idle")
	return response, true
}
