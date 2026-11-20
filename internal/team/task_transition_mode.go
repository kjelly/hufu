package team

import (
	"context"
	"fmt"
	"time"
)

// validateTaskTransitionMode distinguishes normal lifecycle updates, explicit
// worker resets and operator-authorized submission repair. The caller holds
// the projection lock and commits through the same event-first owner.
func (c *Coordinator) validateTaskTransitionMode(ctx context.Context, current *TodoItem, next TaskStatus, metadata map[string]any) (bool, *ResultRepairGrant, error) {
	reset, _ := metadata["reset_for_resume"].(bool)
	release, _ := metadata["release_deferred_dispatch"].(bool)
	if release && (!reset || next != TaskPending || !collectionDispatchDeferred(current, current)) {
		return false, nil, fmt.Errorf("commit task transition: task %s lacks a deferred first dispatch", current.ID)
	}
	grant, _ := metadata["result_repair_grant"].(*ResultRepairGrant)
	if grant != nil {
		if err := c.validateResultRepairGrant(ctx, current, next, grant); err != nil {
			return false, nil, fmt.Errorf("commit task transition: invalid operator result repair grant: %w", err)
		}
	}
	if reset && next != TaskPending {
		return false, nil, fmt.Errorf("commit task transition: task %s resume reset must target pending", current.ID)
	}
	if !reset && grant == nil && !CanTransition(current.Status, next) {
		return false, nil, fmt.Errorf("commit task transition: invalid %s -> %s for task %s", current.Status, next, current.ID)
	}
	return reset, grant, nil
}

func setResumePendingProjection(item *TodoItem, detail string, releaseDeferral bool) {
	if releaseDeferral {
		discardDispatchDeferralFingerprints(item)
	}
	item.Status = TaskPending
	item.Detail = detail
	item.Output = ""
	item.VerifyResult = nil
	item.RuntimeError = nil
	item.FailureEvent = nil
	item.RecoveryState = RecoveryStateNotStarted
	item.LastOperation = ""
	item.Progress = ProgressUnknown
	item.ProgressCriteria = nil
	item.StartedAt = time.Time{}
	item.EndedAt = time.Time{}
	item.ModelTime = 0
	item.ToolTime = 0
}
