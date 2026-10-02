package team

import (
	"context"
	"fmt"
	"log"
)

// withdrawUndispatchedBatch undoes what a failed ExecuteTasks batch left
// behind before any worker started. Its non-duplicate tasks stop counting as
// delegated in this round, and its occurrences that are still pending are
// removed: they never ran, and kept they would make every later dispatch of
// the same work a duplicate. Occurrences the batch already moved to a
// terminal state, such as skipped or blocked, stay as they are.
func (c *Coordinator) withdrawUndispatchedBatch(ctx context.Context, tasks []TaskDef, duplicates map[int]bool, created []*TodoItem, cause error) {
	c.forgetDelegatedTasks(tasks, duplicates)
	var pending []string
	for index, item := range created {
		if item == nil || duplicates[index] {
			continue
		}
		if current := c.todoItemByID(item.ID); current != nil && current.Status == TaskPending {
			pending = append(pending, item.ID)
		}
	}
	if len(pending) == 0 {
		return
	}
	if err := c.CommitTaskRemoval(context.WithoutCancel(ctx), pending...); err != nil {
		log.Printf("warning: withdraw tasks %v that never started: %v", pending, err)
		return
	}
	c.report(c.newEvent("step").withMessage(fmt.Sprintf("withdrew %d task(s) that never started: %v", len(pending), cause)))
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
}

// forgetDelegatedTasks reverses checkDuplicateTasks' round counts for the
// tasks it admitted.
func (c *Coordinator) forgetDelegatedTasks(tasks []TaskDef, duplicates map[int]bool) {
	c.delegatedTasksMu.Lock()
	defer c.delegatedTasksMu.Unlock()
	for index, task := range tasks {
		if duplicates[index] || task.InvariantVerification != "" || task.CatalogAction != nil {
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
