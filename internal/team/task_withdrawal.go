package team

import (
	"context"
	"fmt"
	"log"
)

func (c *Coordinator) checkBatchPreflight(ctx context.Context, items []*TodoItem) error {
	if len(c.session.Config.Preflight) == 0 {
		return nil
	}
	_, err := c.checkCapabilityRequirements(ctx, c.session.Config.Preflight)
	blocked, ok := isCapabilityBlockedError(err)
	if !ok {
		return err
	}
	detail := blocked.Reason
	if detail == "" {
		detail = "capability preflight blocked"
	}
	for _, item := range items {
		c.PersistFailureWithClassAndStatus(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked)
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("needs_human").withMessage(detail))
	c.PersistFailure("coordinator", "capability preflight failed", "", detail)
	return fmt.Errorf("capability preflight failed: %s", detail)
}

func (c *Coordinator) declineTaskBatch(ctx context.Context, items []*TodoItem) error {
	detail := c.FailureDetail(fmt.Errorf("user declined task execution"), FailureSourceUserDeclined)
	for _, item := range items {
		if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskSkipped, detail, "", nil); err != nil {
			return err
		}
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("step").withMessage("Steps: user declined task execution"))
	c.wrapUp.Store(1)
	c.PersistFailure("coordinator", "user declined task execution", "", detail)
	return fmt.Errorf("user declined task execution: call finish immediately with your best summary of work completed so far")
}

// withdrawUndispatchedBatch undoes what a failed ExecuteTasks batch left
// behind before any worker started. Its non-duplicate tasks stop counting as
// delegated in this round, and its occurrences that are still pending are
// removed: they never ran, and kept they would make every later dispatch of
// the same work a duplicate. Occurrences the batch already moved to a
// terminal state, such as skipped or blocked, stay as they are.
func (c *Coordinator) withdrawUndispatchedBatch(ctx context.Context, tasks []TaskDef, duplicates map[int]bool, created []*TodoItem, cause error, atomicBatch bool) error {
	c.forgetDelegatedTasks(tasks, duplicates)
	var pending []*TodoItem
	for index, item := range created {
		if item == nil || duplicates[index] {
			continue
		}
		if current := c.todoItemByID(item.ID); current != nil && current.Status == TaskPending && (current.EvidenceInventoryBinding == nil || current.EvidenceInventoryBinding.SourceTaskID == "") {
			pending = append(pending, current)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	ids := make([]string, len(pending))
	for i, item := range pending {
		ids[i] = item.ID
	}
	if atomicBatch {
		if len(pending) != len(created) {
			return nil
		}
		if err := c.commitTaskRemovalBatch(context.WithoutCancel(ctx), pending, ids); err != nil {
			return fmt.Errorf("withdraw bound task batch %v: %w", ids, err)
		}
	} else if err := c.CommitTaskRemoval(context.WithoutCancel(ctx), ids...); err != nil {
		log.Printf("warning: withdraw tasks %v that never started: %v", ids, err)
		return nil
	}
	c.report(c.newEvent("step").withMessage(fmt.Sprintf("withdrew %d task(s) that never started: %v", len(pending), cause)))
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	return nil
}

// forgetDelegatedTasks reverses checkDuplicateTasks' round counts for the
// tasks it admitted.
func (c *Coordinator) forgetDelegatedTasks(tasks []TaskDef, duplicates map[int]bool) {
	c.delegatedTasksMu.Lock()
	defer c.delegatedTasksMu.Unlock()
	for index, task := range tasks {
		if duplicates[index] || task.EvidenceInventoryBinding != nil || task.InvariantVerification != "" || task.CatalogAction != nil {
			continue
		}
		desc := task.Goal
		if task.Constraints != "" {
			desc += "\nconstraints: " + task.Constraints
		}
		key := duplicateTaskIdentity(task.Agent, desc, task.VerifySpec, task.Verify, task.VerifyMode)
		if c.delegatedTasks[key] > 0 {
			c.delegatedTasks[key]--
		}
	}
}
