package team

import "fmt"

// maxCoordinatorCallsAfterWorkflowFailure bounds the coordinator tool calls a
// run may make while its phase workflow is FAILED. New delegation is forbidden,
// so without a bound the coordinator can spend a long
// stream inspecting the repository before it tries the one delegation that
// ends the run. reconcile_task, which can reopen a workflow failed by a task,
// does not count against the bound.
const maxCoordinatorCallsAfterWorkflowFailure = 3

const reconcileTaskToolName = "reconcile_task"

// workflowFailureResult is the ExecuteTasks result for a batch whose outcome
// left the phase workflow FAILED. A failure caused by a task can still be
// reopened with reconcile_task when a verified replacement already exists;
// otherwise the coordinator can acknowledge a partial finish. A structural failure has no reopening task:
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
	items := c.taskTracker.TodoList().Items()
	var candidates []string
	for _, item := range items {
		if item == nil || item.ID == taskID {
			continue
		}
		resolution := &TaskResolution{Status: "reconciled", ResolvedBy: item.ID, Reason: "check available replacement"}
		if ValidateResolution(resolution, taskID, items, c.executionRunID) == nil {
			candidates = append(candidates, item.ID)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("%w; the workflow is %s and has no verified replacement for task %s. Do not delegate or reconcile unrelated tasks. Call finish with acknowledge_failed_tasks:true for a partial result; after %d other coordinator tool calls the runtime ends the run",
			err, PhaseFailed, taskID, maxCoordinatorCallsAfterWorkflowFailure)
	}
	return fmt.Errorf("%w; the workflow is %s: use %s on task %s with a verified replacement from %v, or call finish with acknowledge_failed_tasks:true for a partial result. New delegation is forbidden; after %d other coordinator tool calls the runtime ends the run",
		err, PhaseFailed, reconcileTaskToolName, taskID, candidates, maxCoordinatorCallsAfterWorkflowFailure)
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
