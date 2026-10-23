package team

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

func (t *finishTool) finishWorkflow(ctx context.Context, acknowledged bool) (fantasy.ToolResponse, bool, error) {
	w := t.coordinator.phaseWorkflow
	if w == nil {
		return fantasy.ToolResponse{}, false, nil
	}
	if acknowledged && w.State() == PhaseFailed {
		response, err := t.finishFailedWorkflow(ctx)
		return response, true, err
	}
	if err := w.requireFinished(); err != nil {
		return fantasy.NewTextErrorResponse("cannot finish: " + err.Error()), true, nil
	}
	return fantasy.ToolResponse{}, false, nil
}

// Acknowledging a failed workflow terminates through the canonical partial
// path. It cannot reopen dispatch, render an unaudited report, or run rollback.
func (t *finishTool) finishFailedWorkflow(ctx context.Context) (fantasy.ToolResponse, error) {
	c := t.coordinator
	items := c.taskTracker.TodoList().Items()
	if pending := pendingTodoItems(items); len(pending) > 0 {
		return fantasy.NewTextErrorResponse("cannot finish while worker tasks are unresolved or still running:\n" + formatPendingTasks(pending)), nil
	}
	if c.terminalSessionMgr != nil {
		if err := c.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			return fantasy.NewTextErrorResponse("cannot finish while terminal sessions remain unresolved: " + err.Error()), nil
		}
	}
	c.SetWrapUp()
	c.acceptanceRecovery.Store(false)
	const reason = "workflow failed; ending with an acknowledged partial result"
	response := c.summaryFromTodos(errors.New(reason))
	acceptance, acceptanceErr := c.runAcceptance(ctx, response)
	if acceptanceErr != nil {
		response += "\n\nAcceptance failed: " + acceptanceErr.Error()
	}
	if _, err := c.buildEvidenceManifest(ctx, false); err != nil {
		c.report(c.newEvent("error").withMessage("partial evidence manifest generation failed: " + err.Error()))
	}
	if err := c.finalizeEvidenceManifest(ctx, acceptance); err != nil {
		c.report(c.newEvent("error").withMessage("partial evidence manifest finalization failed: " + err.Error()))
	}
	state := AcceptanceNotConfigured
	if acceptance != nil {
		state = acceptance.State
	}
	unresolved := UnresolvedTaskReferences(items)
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: unresolved, Acceptance: state, Reason: reason, RunFailed: len(unresolved) == 0,
		Response: response, Stats: SummarizeRunStats(items), Metrics: c.Metrics(), GoalMode: c.GoalMode(),
	})
	// Preserve the evaluator's cancelled/blocked outcomes. A structural failure
	// with no unresolved task is still a run failure, never a completed claim.
	evaluated.Acceptance = acceptance
	c.lastEvidenceManifestMu.RLock()
	evaluated.EvidenceManifest = c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if response, stop := t.prepareTerminalFinish(ctx, true, &evaluated, acceptance); stop {
		return response, nil
	}
	c.finishCalled.Store(true)
	c.coordinatorPolicyRepairPending.Store(false)
	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", response)), nil
}
