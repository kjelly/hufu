package team

import (
	"context"
	"errors"
	"fmt"
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

func validateStoppedWorkClaims(work *resultCollectionWork, input SubmitResultInput) error {
	if work.StopReason == workStopToolLoop && input.Status == TaskResultStatusSuccess {
		return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("work stopped at a tool loop; preserve that limitation and use completed_with_gaps, partial, blocked or failed, not success"))
	}
	return nil
}
