package team

import (
	"fmt"
	"time"
)

// failRuntimeAction records a structured action failure on the todo, in the
// failure journal, and on the action lifecycle stream, and returns err.
func (c *Coordinator) failRuntimeAction(task TaskDef, todoID, actionID string, startedAt time.Time, err error) error {
	runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
	_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
	c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
	c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
	return err
}

// canonicalizeRuntimeActionOutputs validates a provider's outputs before any
// declared artifact is ingested or a workset projection is written, so a
// rejected result leaves no ingested artifact or current-workset.json behind.
func (c *Coordinator) canonicalizeRuntimeActionOutputs(task TaskDef, todoID, actionID string, startedAt time.Time, result ActionResult) (map[string]any, string, error) {
	outputs, hash, err := CanonicalizeRuntimeOutputs(result.Outputs)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize structured action outputs: %w", c.failRuntimeAction(task, todoID, actionID, startedAt, err))
	}
	if err := c.validateCatalogActionOutputs(task, outputs); err != nil {
		return nil, "", c.failRuntimeAction(task, todoID, actionID, startedAt, err)
	}
	return outputs, hash, nil
}
