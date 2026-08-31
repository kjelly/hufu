package team

import (
	"context"
	"fmt"
	"strings"
)

// TargetedRecoveryAction identifies an operator-requested recovery operation.
type TargetedRecoveryAction string

const (
	TargetedRecoveryReconcile TargetedRecoveryAction = "reconcile"
	TargetedRecoveryRetry     TargetedRecoveryAction = "retry"
)

// TargetedRecoveryReport is the bounded, machine-readable result of a
// reconcile or retry operation. The task and run outcome are projections of
// the canonical task/event state; callers must not infer success from prose.
type TargetedRecoveryReport struct {
	Action        TargetedRecoveryAction `json:"action"`
	TaskID        string                 `json:"task_id"`
	Status        TaskStatus             `json:"status,omitzero"`
	RecoveryState string                 `json:"recovery_state,omitzero"`
	Retries       int                    `json:"retries,omitzero"`
	Detail        string                 `json:"detail,omitzero"`
	RunID         string                 `json:"run_id,omitzero"`
	RunOutcome    RunOutcome             `json:"run_outcome,omitzero"`
}

// ReconcileTask runs the task's declared read-only reconciliation or
// verification contract without redispatching the worker. A completed
// reconciliation is promoted to done only after any affected acceptance
// criteria are revalidated through the canonical transition path.
func (c *Coordinator) ReconcileTask(ctx context.Context, taskID string) (TargetedRecoveryReport, error) {
	return c.runTargetedRecovery(ctx, taskID, TargetedRecoveryReconcile)
}

// RetryTask explicitly retries one terminal task. The centralized repair
// controller decides whether the task is replayable and the event-first reset
// boundary is committed before the worker can run again.
func (c *Coordinator) RetryTask(ctx context.Context, taskID string) (TargetedRecoveryReport, error) {
	return c.runTargetedRecovery(ctx, taskID, TargetedRecoveryRetry)
}

func (c *Coordinator) runTargetedRecovery(ctx context.Context, taskID string, action TargetedRecoveryAction) (TargetedRecoveryReport, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return TargetedRecoveryReport{Action: action}, fmt.Errorf("task id is required")
	}
	if action != TargetedRecoveryReconcile && action != TargetedRecoveryRetry {
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, fmt.Errorf("unsupported targeted recovery action %q", action)
	}
	if c == nil {
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, fmt.Errorf("coordinator is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	invocationCtx, end, err := c.beginPublicInvocationExecutionRun(ctx)
	if err != nil {
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, err
	}
	defer end()

	if err := c.checkRunAdmission(); err != nil {
		c.finalizePublicInvocationFailure(err)
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, err
	}
	if err := c.ValidateWorkspaceIsolation(); err != nil {
		c.finalizePublicInvocationFailure(err)
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, err
	}
	if err := c.ValidateResourceLocks(invocationCtx); err != nil {
		c.finalizePublicInvocationFailure(err)
		return TargetedRecoveryReport{Action: action, TaskID: taskID}, err
	}
	if action == TargetedRecoveryRetry {
		if err := c.startProviderExecutionBoundary(invocationCtx); err != nil {
			c.finalizePublicInvocationFailure(err)
			return TargetedRecoveryReport{Action: action, TaskID: taskID}, err
		}
	}

	c.resetRoundState()
	c.reconcileTaskStatusProjection()
	c.initTaskJournal()
	c.ResumeContinuationCheckpoint()

	item := c.todoItemByID(taskID)
	if item == nil {
		return c.finishTargetedRecovery(invocationCtx, action, taskID, fmt.Errorf("task %s not found", taskID))
	}

	var operationErr error
	switch action {
	case TargetedRecoveryReconcile:
		operationErr = c.reconcileTaskForOperator(invocationCtx, item)
	case TargetedRecoveryRetry:
		operationErr = c.retryTaskForOperator(invocationCtx, item)
	}
	return c.finishTargetedRecovery(invocationCtx, action, taskID, operationErr)
}

func (c *Coordinator) reconcileTaskForOperator(ctx context.Context, item *TodoItem) error {
	if item == nil {
		return fmt.Errorf("task is nil")
	}
	if item.ReconcileTool == "" && item.Verify == "" && item.VerifySpec == nil {
		return fmt.Errorf("task %s has no declared reconcile_tool, verify, or verify_spec", item.ID)
	}

	state := c.reconcileInterruptedTask(ctx, item)
	c.taskTracker.TodoList().SetRecoveryState(item.ID, state)
	_ = c.emitEvent("recovery_decision", "operator", item.ID, map[string]any{
		"action":         string(TargetedRecoveryReconcile),
		"recovery_state": state,
		"worker_replay":  false,
	})

	switch state {
	case RecoveryStateComplete:
		if item.Status == TaskDone {
			return nil
		}
		if !CanTransition(item.Status, TaskDone) {
			return fmt.Errorf("task %s reconciliation completed but status %s cannot transition to done", item.ID, item.Status)
		}
		if err := c.revalidateRecoveryCriteria(ctx, item); err != nil {
			return fmt.Errorf("task %s reconciliation criteria failed: %w", item.ID, err)
		}
		return c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskDone, "operator reconciliation confirmed task was completed", "", nil)
	case RecoveryStateNotStarted:
		// Keep the task unresolved. A separate explicit retry is required so an
		// operator can review the evidence before replaying the operation.
		return nil
	case RecoveryStatePartial, RecoveryStateUnknown:
		detail := fmt.Sprintf("operator reconciliation found task state: %s", state)
		if item.Status != TaskBlocked {
			if err := c.PersistFailureWithClassAndStatusError(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked); err != nil {
				return fmt.Errorf("block task %s after reconciliation: %w", item.ID, err)
			}
		}
		return fmt.Errorf("task %s reconciliation state is %s; retry is blocked", item.ID, state)
	default:
		return fmt.Errorf("task %s returned unsupported reconciliation state %q", item.ID, state)
	}
}

func (c *Coordinator) retryTaskForOperator(ctx context.Context, item *TodoItem) error {
	if item == nil {
		return fmt.Errorf("task is nil")
	}
	canRetryNotStarted := item.Status == TaskInProgress && item.RecoveryState == RecoveryStateNotStarted
	if item.Status != TaskError && item.Status != TaskBlocked && !canRetryNotStarted {
		return fmt.Errorf("task %s has status %s; targeted retry accepts error/blocked tasks or a not_started reconciliation", item.ID, item.Status)
	}

	task := taskDefFromTodoItem(item)
	request := RepairRequest{
		Task:          task,
		Attempt:       item.Retries,
		MaxAttempts:   item.MaxRetries,
		RecoveryState: item.RecoveryState,
	}
	decision := c.RepairController().Decide(request)
	_ = c.emitEvent("repair_decision", "operator", item.ID, map[string]any{
		"action":  string(decision.Action),
		"reason":  decision.Reason,
		"attempt": item.Retries,
	})
	if decision.Action != RepairRetry {
		return fmt.Errorf("task %s cannot be retried: %s", item.ID, decision.Reason)
	}

	repair := c.RepairController().Execute(ctx, RepairRequest{
		Task:          task,
		Attempt:       item.Retries,
		MaxAttempts:   item.MaxRetries,
		RecoveryState: item.RecoveryState,
		Checkpoint: func(checkpointCtx context.Context) error {
			return c.CommitTaskResetForRetry(checkpointCtx, item.ID, "operator requested targeted retry")
		},
		Retry: func(retryCtx context.Context) error {
			_, err := c.executeTask(retryCtx, task, item.ID)
			return err
		},
	})
	if repair.Err != nil {
		return repair.Err
	}
	current := c.todoItemByID(item.ID)
	if current == nil || current.Status != TaskDone {
		status := "missing"
		if current != nil {
			status = string(current.Status)
		}
		return fmt.Errorf("targeted retry for task %s finished without done status (status: %s)", item.ID, status)
	}
	return nil
}

func (c *Coordinator) finishTargetedRecovery(ctx context.Context, action TargetedRecoveryAction, taskID string, operationErr error) (TargetedRecoveryReport, error) {
	response := fmt.Sprintf("targeted %s for task %s", action, taskID)
	if operationErr != nil {
		response += " failed: " + operationErr.Error()
	}
	items := c.taskTracker.TodoList().Items()
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: UnresolvedTaskReferences(items),
		RunFailed:       operationErr != nil,
		Response:        response,
		Reason:          "targeted recovery",
		Stats:           SummarizeRunStats(items),
		Metrics:         c.Metrics(),
		GoalMode:        c.GoalMode(),
	})
	result := c.FinalizeRun(ctx, &evaluated, nil)
	report := c.targetedRecoveryReport(action, taskID, result)
	if operationErr != nil {
		return report, operationErr
	}
	if result != nil && !c.TerminalLifecycleConfirmed() {
		return report, fmt.Errorf("targeted recovery terminal persistence is unconfirmed")
	}
	return report, nil
}

func (c *Coordinator) targetedRecoveryReport(action TargetedRecoveryAction, taskID string, result *RunResult) TargetedRecoveryReport {
	report := TargetedRecoveryReport{Action: action, TaskID: taskID}
	if item := c.todoItemByID(taskID); item != nil {
		report.Status = item.Status
		report.RecoveryState = item.RecoveryState
		report.Retries = item.Retries
		report.Detail = item.Detail
	}
	if result != nil {
		report.RunID = result.RunID
		report.RunOutcome = result.Outcome
	}
	return report
}
