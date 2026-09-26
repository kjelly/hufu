package team

import "fmt"

// maxCoordinatorCallsAfterWorkflowFailure bounds the coordinator tool calls a
// run may make while its phase workflow is FAILED. Delegation and finish can
// no longer succeed there, so without a bound the coordinator can spend a long
// stream inspecting the repository before it tries the one delegation that
// ends the run. reconcile_task, which can reopen a workflow failed by a task,
// does not count against the bound.
const maxCoordinatorCallsAfterWorkflowFailure = 3

const reconcileTaskToolName = "reconcile_task"

// workflowFailureResult is the ExecuteTasks result for a batch whose outcome
// left the phase workflow FAILED. A failure caused by a task can still be
// reopened with reconcile_task, so the error tells the coordinator exactly
// that and how many calls remain. A structural failure has no reopening task:
// the run enters wrap-up and the stream ends, so the runtime writes the
// LLM-free partial summary at once.
func (c *Coordinator) workflowFailureResult(err error) error {
	if c == nil || c.phaseWorkflow == nil || c.phaseWorkflow.State() != PhaseFailed {
		return err
	}
	taskID := c.phaseWorkflow.failedTask()
	if taskID == "" {
		c.wrapUp.Store(1)
		return markCoordinatorFatal(fmt.Errorf("%w; the workflow cannot be reopened, so the run ends", err))
	}
	return fmt.Errorf("%w; the workflow is now %s, so delegation and finish can no longer succeed: the only call that can reopen it is %s on task %s, and after %d other coordinator tool calls the run ends and the runtime writes the partial summary",
		err, PhaseFailed, reconcileTaskToolName, taskID, maxCoordinatorCallsAfterWorkflowFailure)
}

// failedWorkflowCallStop enforces maxCoordinatorCallsAfterWorkflowFailure
// before a coordinator tool runs. Past the bound the run enters wrap-up and the
// call is refused with a stream-ending error; with failed tasks on record,
// wrap-up recovery then finalizes the run without another model turn.
func (c *Coordinator) failedWorkflowCallStop(toolName string) error {
	if c == nil {
		return nil
	}
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.phaseWorkflow.State() != PhaseFailed {
		c.coordinatorCallsAfterWorkflowFailure.Store(0)
		return nil
	}
	if toolName == reconcileTaskToolName {
		return nil
	}
	if c.coordinatorCallsAfterWorkflowFailure.Add(1) <= maxCoordinatorCallsAfterWorkflowFailure {
		return nil
	}
	c.wrapUp.Store(1)
	return fmt.Errorf("%w: the workflow is %s and %d coordinator tool calls followed without %s; tool %q was not executed and the run ends",
		errCoordinatorToolFailure, PhaseFailed, maxCoordinatorCallsAfterWorkflowFailure, reconcileTaskToolName, toolName)
}
