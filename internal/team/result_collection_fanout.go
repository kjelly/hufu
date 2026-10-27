package team

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/execution"
)

// Completed parent evidence lives in CAS. Keep private leaf workspaces through
// collection and verification recovery, and release only those exact workspaces
// after the parent's canonical terminal transition succeeds.
func (c *Coordinator) releaseFanoutCollectionWorkspaces(ctx context.Context, taskID string) {
	item := c.todoItemByID(taskID)
	if item == nil || item.Status != TaskDone {
		return
	}
	root := filepath.Join(c.artifactStoreRootPath(), "extra-models")
	for _, receipt := range item.ExecutionReceipts {
		checkpoint := receipt.ResultCollection
		if checkpoint == nil || !checkpoint.FanoutLeaf || checkpoint.State != ResultCollectionCommitted {
			continue
		}
		work, err := c.loadCollectionWork(ctx, item, checkpoint)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, work.Workspace)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			continue
		}
		if err := os.RemoveAll(work.Workspace); err != nil {
			log.Printf("warning: release completed fanout collection workspace: %v", err)
		}
	}
}

// Select by the frozen topology slot, including repeated model names. A
// missing work checkpoint after an interrupted fanout is unknown-effect work,
// never permission to silently dispatch that leaf again.
func (c *Coordinator) fanoutCollectionResume(ctx context.Context, taskID string, index int, target execution.ExecutionTarget) (*ResultCollectionCheckpoint, *resultCollectionWork, error) {
	item := c.todoItemByID(taskID)
	if currentResultCollection(item) == nil {
		return nil, nil, nil
	}
	var selected *ResultCollectionCheckpoint
	for _, receipt := range item.ExecutionReceipts {
		cp := receipt.ResultCollection
		if cp == nil || !cp.FanoutLeaf || cp.FanoutIndex != index || cp.ExecutionTarget != target {
			continue
		}
		if selected == nil || cp.Attempt > selected.Attempt {
			selected = cloneResultCollectionCheckpoint(cp)
		}
	}
	if selected == nil {
		return nil, nil, &resultCollectionFailure{Reason: RepairFailureSource, Cause: fmt.Errorf("fanout leaf has no sealed work checkpoint; execution effects require reconciliation")}
	}
	work, err := c.loadCollectionWork(ctx, item, selected)
	return selected, work, err
}
