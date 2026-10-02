package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

type reconcileTaskTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *reconcileTaskTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reconcile_task",
		Description: "Mark a failed or blocked task as resolved by a later done task that replaced or fixed it, removing it from the unresolved failed tasks finish gate. A task that nothing replaced, such as a step the user declined or skipped, cannot be reconciled; call finish with acknowledge_failed_tasks:true instead.",
		Parameters: map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "Todo ID of the failed or blocked task, exactly as listed (for example \"5\")",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "Resolution status: superseded or reconciled",
				"enum":        []string{"superseded", "reconciled"},
			},
			"resolved_by": map[string]any{
				"type":        "string",
				"description": "Todo ID of the done task that replaced or fixed the failed task (for example \"7\"); never a label or a reason",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Explanation of how the issue was fixed or why the task was superseded",
			},
			"evidence": map[string]any{
				"type":        "array",
				"description": "Optional list of objective evidence references verifying resolution",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"value":       map[string]any{"type": "string"},
					},
				},
			},
		},
		Required: []string{"task_id", "status", "resolved_by", "reason"},
	}
}

func (t *reconcileTaskTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TaskID     string        `json:"task_id"`
		Status     string        `json:"status"`
		ResolvedBy string        `json:"resolved_by"`
		Reason     string        `json:"reason"`
		Evidence   []EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	// Security: strip model-injected HMAC signatures from tool input to prevent forged signatures
	for i := range args.Evidence {
		args.Evidence[i].SystemHMAC = ""
	}
	res := &TaskResolution{
		Status:     args.Status,
		ResolvedBy: args.ResolvedBy,
		Reason:     args.Reason,
		Evidence:   args.Evidence,
	}
	if err := t.coordinator.CommitTaskResolution(ctx, args.TaskID, res); err != nil {
		var items []*TodoItem
		if t.coordinator.taskTracker != nil && t.coordinator.taskTracker.TodoList() != nil {
			items = t.coordinator.taskTracker.TodoList().Items()
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to reconcile task: %v%s", err, reconcileTaskHint(err, args.TaskID, items))), nil
	}
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))
	return fantasy.NewTextResponse(fmt.Sprintf("task %s marked as %s by task %s: %s", args.TaskID, args.Status, args.ResolvedBy, args.Reason)), nil
}

// unknownTaskError reports a reconcile_task task_id that names no todo item.
type unknownTaskError struct {
	taskID string
}

func (e *unknownTaskError) Error() string {
	return fmt.Sprintf("task_id %q is not in the todo list", e.taskID)
}

// unknownResolverError reports a resolved_by that is empty or names no todo
// item. A model that reached for reconcile_task to close a step nothing
// replaced lands here, so reconcile_task can point it at finish instead.
type unknownResolverError struct {
	status     string
	resolvedBy string
	taskID     string
}

func (e *unknownResolverError) Error() string {
	if e.resolvedBy == "" {
		return fmt.Sprintf("resolution status %q requires resolved_by: the ID of the done task that replaced or fixed task %s", e.status, e.taskID)
	}
	return fmt.Sprintf("resolved_by %q is not a task in the todo list; it must be the ID of the done task that replaced or fixed task %s", e.resolvedBy, e.taskID)
}

// reconcileTaskHint tells the coordinator how to correct a reconcile_task call
// whose IDs name no todo item; other rejections need no hint.
func reconcileTaskHint(err error, taskID string, items []*TodoItem) string {
	var unknownTask *unknownTaskError
	if errors.As(err, &unknownTask) {
		failed := failedTodoItems(items)
		if len(failed) == 0 {
			return "\nNo task is failed or blocked."
		}
		ids := make([]string, len(failed))
		for i, item := range failed {
			ids[i] = item.ID
		}
		return "\nFailed or blocked tasks: " + strings.Join(ids, ", ") + "."
	}
	var unknownResolver *unknownResolverError
	if errors.As(err, &unknownResolver) {
		return fmt.Sprintf("\nIf no task replaced or fixed task %s (for example the user declined or skipped that step), it cannot be reconciled: call finish with acknowledge_failed_tasks:true and report the step as not done.", taskID)
	}
	return ""
}
