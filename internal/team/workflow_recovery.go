package team

import "context"

// reconcileRecoveredWorkflow closes the checkpoint gap between task completion
// and ExecuteTasks' phase observation. Result-only recovery also completes tasks
// outside ExecuteTasks, so it must settle the same workflow gate before the
// coordinator builds its phase-scoped delegation tools.
func (c *Coordinator) reconcileRecoveredWorkflow(ctx context.Context) error {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.taskTracker == nil {
		return nil
	}
	w := c.phaseWorkflow
	items := c.taskTracker.TodoList().Items()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		phase, results, _, _ := w.snapshot()
		if phase == PhaseInit || phase == PhaseDone || phase == PhaseFailed {
			return nil
		}
		// An optional critic deliberately keeps a successful VERIFY phase open.
		// Repeated resumes must not emit another success for the same gate.
		if phase == PhaseVerify && results[phase].Status == PhaseStatusSuccess {
			return nil
		}
		for _, item := range items {
			if item == nil || item.Phase != phase || (!w.phaseContracts[phase][item.ContractID] && !w.phaseOptionalContracts[phase][item.ContractID]) {
				continue
			}
			if isUnresolvedTaskStatus(item.Status) && VerifiedTaskResolution(item, items, resolutionRunID(items, item)) == nil {
				// Recovery policy owns unfinished tasks. Do not turn a pending or
				// operator-blocked occurrence into a workflow failure on resume.
				return nil
			}
		}
		if err := w.observe(items); err != nil {
			c.saveCheckpoint()
			return c.workflowFailureResult(err)
		}
		next, observed, _, _ := w.snapshot()
		if next != phase || observed[phase].Status != results[phase].Status {
			c.saveCheckpoint()
		}
		if next == phase {
			return nil
		}
	}
}
