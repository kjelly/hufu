package team

import (
	"context"
	"fmt"
)

// resolveRuntimeMediaValue returns canonical native evidence without modifying
// Outputs. A schema-selected metadata value grants no provider byte access.
func (c *Coordinator) resolveRuntimeMediaValue(taskID string) (any, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil || c.session == nil {
		return nil, fmt.Errorf("runtime media source occurrences are unavailable")
	}
	items := c.taskTracker.TodoList().Items()
	runID := coordinatorRuntimeRunID(c)
	origins, err := c.recoveredTaskOutputOrigins(items)
	if err != nil {
		return nil, err
	}
	source, err := uniqueTaskOutputSource(items, taskID, runID, origins)
	if err != nil {
		return nil, err
	}
	if source.Status != TaskDone || source.Action == nil || source.TypedResult == nil || source.TypedResult.Source != "runtime" || source.TypedResult.RuntimeMedia == nil {
		return nil, fmt.Errorf("runtime media source is not a completed native action result")
	}
	result := cloneTaskResult(source.TypedResult)
	if err := validateCompletedTaskResult(result); err != nil {
		return nil, err
	}
	if err := validateRuntimeTaskResult(result); err != nil {
		return nil, err
	}
	sourceRun := taskOutputOriginRun(runID, source.ID, []map[string]string{origins})
	receipt := latestSuccessfulExecutionReceipt(source, sourceRun)
	if receipt == nil || receipt.ActionInvocationID == "" || receipt.RuntimeOutputsHash != result.RuntimeOutputsHash ||
		receipt.RunInputSnapshotID != source.RunInputSnapshotID || receipt.RunInputSnapshotHash != source.RunInputSnapshotHash ||
		result.RunInputSnapshotID != source.RunInputSnapshotID || result.RunInputSnapshotHash != source.RunInputSnapshotHash ||
		receipt.MaterializedActionPayloadHash != source.MaterializedActionPayloadHash || result.MaterializedActionPayloadHash != source.MaterializedActionPayloadHash ||
		!equalStringMaps(receipt.BoundInputs, source.BoundInputs) || !equalStringMaps(result.BoundInputs, source.BoundInputs) {
		return nil, fmt.Errorf("runtime media source has no matching native action receipt")
	}
	if source.RunInputSnapshotID != "" {
		snapshot := c.RunInputSnapshot()
		if snapshot == nil || snapshot.ID != source.RunInputSnapshotID || snapshot.SnapshotHash != source.RunInputSnapshotHash {
			return nil, fmt.Errorf("runtime media source belongs to a different frozen input snapshot")
		}
	} else if sourceRun != runID || source.RunInputSnapshotHash != "" || source.MaterializedActionPayloadHash != "" || len(source.BoundInputs) != 0 {
		return nil, fmt.Errorf("unbound runtime media sources are limited to their original invocation")
	}
	// This mirrors the existing synchronous runtime-input lookup API. Reading
	// the validated durable journal also works after the live store has closed;
	// historical bindings never rebuild themselves from a newer source result.
	events, err := readValidatedEventJournal(context.Background(), c.session.Workspace)
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTreeContext(context.Background(), c.session.Workspace)
	if err != nil {
		return nil, err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, c.activeBranchID())
	if err != nil {
		return nil, err
	}
	if err := verifyRuntimeMediaResult(result, receipt, lineage); err != nil {
		return nil, err
	}
	return result.RuntimeMedia.clone(), nil
}
