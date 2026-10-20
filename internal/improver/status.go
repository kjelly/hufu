package improver

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/team"
)

// StageTasks is a presentation-only mapping of parent stages to the existing
// task dashboard. These logical stage IDs never replace child receipt IDs.
func StageTasks(state LedgerState) []*team.TodoItem {
	items := make([]*team.TodoItem, 0, len(state.Stages))
	for _, id := range slices.Sorted(maps.Keys(state.Stages)) {
		stage := state.Stages[id]
		status := team.TaskBlocked
		switch stage.State {
		case "planned":
			status = team.TaskPending
		case "admitted", "bound", "started":
			status = team.TaskInProgress
		case "cancelled", "skipped", "skipped_budget":
			status = team.TaskSkipped
		case "failed":
			status = team.TaskError
		case "completed":
			if len(stage.Attempts) > 0 {
				result := stage.Attempts[len(stage.Attempts)-1].Settlement
				if result != nil && result.UsageKnown {
					switch result.Outcome {
					case "passed":
						status = team.TaskDone
					case "failed", "timed_out", "invalid_proposal":
						status = team.TaskError
					case "cancelled", "not_dispatched":
						status = team.TaskSkipped
					}
				}
			}
		}
		description := stage.Spec.Kind + " " + stage.Spec.Group
		if stage.Spec.Kind == "executor" {
			description = fmt.Sprintf("%s / %s / replicate %d", stage.Spec.Slot.Variant, stage.Spec.Slot.Case, stage.Spec.Slot.Replicate)
		}
		item := &team.TodoItem{ID: id, Agent: stage.Spec.Kind, Desc: description,
			Status: status, Detail: stage.Reason, Source: "improver"}
		if len(stage.Attempts) > 0 {
			attempt := stage.Attempts[len(stage.Attempts)-1]
			item.StartedAt = attempt.AdmittedAt
			if attempt.Identity != nil {
				item.Output = fmt.Sprintf("Child run: %s\nInvocation: %s\nBranch: %s",
					attempt.Identity.RunID, attempt.Identity.InvocationID, attempt.Identity.BranchID)
			}
		}
		items = append(items, item)
	}
	return items
}
