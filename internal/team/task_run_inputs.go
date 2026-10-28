package team

import (
	"errors"
	"fmt"
)

var errTaskInputDrift = errors.New("execution_input_drift")

// bindTaskRunInputs freezes invocation identity for every task, independently
// of whether an action materializes any values. All creation paths call this
// before decision admission; persistence checks the resulting binding again.
// Existing bindings are validated, never overwritten with a newer snapshot.
func (c *Coordinator) bindTaskRunInputs(task TaskDef) (TaskDef, error) {
	snapshot := c.RunInputSnapshot()
	if snapshot == nil {
		if task.RunInputSnapshotID != "" || task.RunInputSnapshotHash != "" || (c != nil && c.session != nil && len(c.session.RunInputDefinitions) > 0) {
			return task, fmt.Errorf("%w: task %q requires its original frozen input snapshot", errTaskInputDrift, task.ID)
		}
		// Low-level callers without an invocation retain their unbound form.
		// Public invocations persist even an empty snapshot before dispatch.
		return task, nil
	}
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		return task, fmt.Errorf("%w: invalid frozen inputs: %v", errTaskInputDrift, err)
	}
	if c.session == nil || snapshot.Team != c.session.Config.Name {
		return task, fmt.Errorf("%w: frozen inputs belong to another team", errTaskInputDrift)
	}
	schemaHash, err := RunInputSchemaHash(c.session.RunInputDefinitions)
	if err != nil || snapshot.SchemaHash != schemaHash {
		return task, fmt.Errorf("%w: frozen input schema differs from the admitted team", errTaskInputDrift)
	}
	if (task.RunInputSnapshotID != "" || task.RunInputSnapshotHash != "") &&
		(task.RunInputSnapshotID != snapshot.ID || task.RunInputSnapshotHash != snapshot.SnapshotHash) {
		return task, fmt.Errorf("%w: task %q is bound to different frozen inputs", errTaskInputDrift, task.ID)
	}
	task.RunInputSnapshotID, task.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	return task, nil
}

func (c *Coordinator) validateTaskRunInputs(item *TodoItem) error {
	bound, err := c.bindTaskRunInputs(taskDefFromTodoItem(item))
	if err != nil {
		return err
	}
	if bound.RunInputSnapshotID != item.RunInputSnapshotID || bound.RunInputSnapshotHash != item.RunInputSnapshotHash {
		return fmt.Errorf("%w: occurrence is missing its admitted snapshot binding", errTaskInputDrift)
	}
	return nil
}

// An empty input context carries no values to re-resolve on a later turn.
// Retain its canonical identity and origin so completed sources stay bound.
// Legacy no-input conversations remain unbound; manufacturing a snapshot now
// would give neither their old work nor their old receipts historical proof.
func (c *Coordinator) reuseEmptyRunInputContext(assignments []RunInputAssignment) (bool, error) {
	if len(c.session.RunInputDefinitions) != 0 || len(assignments) != 0 || c.requestAcceptanceEnabled() {
		return false, nil
	}
	if snapshot := c.RunInputSnapshot(); snapshot != nil {
		schemaHash, err := RunInputSchemaHash(nil)
		if err != nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.Team != c.session.Config.Name || len(snapshot.Inputs) != 0 || snapshot.SchemaHash != schemaHash {
			return true, fmt.Errorf("%w: no-input context has incompatible frozen inputs", errTaskInputDrift)
		}
		return true, c.selectRecoveryRunInputSnapshot(snapshot)
	}
	legacy := false
	if c.taskTracker != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item == nil || IsPrimaryOccurrence(item) {
				continue
			}
			if item.RunInputSnapshotID != "" || item.RunInputSnapshotHash != "" || len(item.BoundInputs) != 0 {
				return true, fmt.Errorf("%w: original no-input context snapshot is unavailable", errTaskInputDrift)
			}
			legacy = true
		}
	}
	return legacy, nil
}
