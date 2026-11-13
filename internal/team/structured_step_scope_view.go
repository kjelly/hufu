package team

import (
	"context"
	"fmt"
)

// A registry is a cache of selected-lineage evidence. Result validation and
// failure diagnostics additionally select the exact currently authorized
// occurrence, so historical attempts cannot satisfy a new task's claim.
func (c *Coordinator) taskStepReceiptRegistry(taskID string, attempt int) (*ExecutionStepReceiptRegistry, error) {
	registry := c.executionStepReceiptRegistry()
	if c.EventStore() == nil {
		return registry, nil
	}
	view := NewExecutionStepReceiptRegistry()
	item := c.todoItemByID(taskID)
	if item == nil {
		return view, nil
	}
	runID, branchID, revision := c.executionRunID, c.EventStore().BranchID(), item.OccurrenceRevision
	if identity, ok := c.activeTaskResultOccurrence(taskID); ok {
		runID = identity.RunID
		if cp := currentResultCollection(item); cp != nil && cp.SourceRunID == identity.RunID && cp.Attempt == attempt &&
			cp.SubmissionRevision == identity.OccurrenceRevision && cp.SubmissionDispatchID == identity.DispatchID {
			if _, err := c.loadCollectionWork(context.Background(), item, cp); err != nil {
				return view, fmt.Errorf("validate structured step recovery source: %w", err)
			}
			branchID, revision = cp.BranchID, cp.WorkOccurrenceRevision
		}
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	for _, receipt := range registry.receipts {
		if receipt.TaskID != taskID || receipt.Attempt != attempt {
			continue
		}
		// A durable occurrence cannot borrow unscoped process-local evidence.
		if receipt.RunID != runID || receipt.BranchID != branchID || receipt.OccurrenceRevision != revision {
			continue
		}
		if err := view.Record(receipt); err != nil {
			return view, err
		}
	}
	return view, nil
}
