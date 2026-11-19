package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const workStopToolLoop = "tool_loop"

// A data-only collection may describe already performed work; it cannot make
// an unsafe operation replayable or complete a closed execution sequence.
func collectableToolLoop(ctx context.Context, err error, task TaskDef, dispositions *attemptToolDispositions) bool {
	loop, ok := errors.AsType[*toolLoopError](err)
	if !ok || loop.FailureClassOverride() != FailureExecution || ctx.Err() != nil ||
		len(task.Execution.ToolSequence) != 0 || task.PlanFirst && task.PlanID == "" ||
		(task.SideEffect != SideEffectNone && task.SideEffect != SideEffectBrowserInteraction) {
		return false
	}
	for _, disposition := range dispositions.snapshot() {
		if disposition.Kind == ToolExecutionPolicyDenied || disposition.Kind == ToolExecutionGuardDenied ||
			disposition.Kind == ToolExecutionCancelled || disposition.Kind == ToolExecutionBudgetExceeded {
			return false
		}
	}
	return true
}

func stoppedWorkStatuses(runtimeCompletes bool) []string {
	statuses := []string{TaskResultStatusPartial, TaskResultStatusBlocked, TaskResultStatusFailed}
	if !runtimeCompletes {
		return append(statuses, TaskResultStatusCompletedWithGaps)
	}
	return statuses
}

func validateStoppedWorkClaims(work *resultCollectionWork, input SubmitResultInput, runtimeCompletes bool) error {
	if work.StopReason != workStopToolLoop || slices.Contains(stoppedWorkStatuses(runtimeCompletes), input.Status) {
		return nil
	}
	return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf(
		"work stopped at a tool loop; preserve that limitation and use %s", strings.Join(stoppedWorkStatuses(runtimeCompletes), ", ")))
}
