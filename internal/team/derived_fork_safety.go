package team

import (
	"encoding/json/v2"
	"fmt"
)

// A derived fork discards executable parent work. It must not discard the
// uncertainty that an external operation or isolated apply still needs the
// operator to reconcile. These checks consume runtime metadata only and never
// run recovery, a verifier, a worker, or a model.
func validateDerivedForkSessionSafety(session *SessionData) error {
	if session == nil || session.WorkflowState == "" {
		return fmt.Errorf("derived fork requires a canonical workflow source")
	}
	if session.DerivedFork != nil && session.DerivedFork.AwaitingNewRequest {
		return fmt.Errorf("derived fork source is awaiting its first new request")
	}
	if session.RecoveryRequired || session.PendingTerminalCommit != nil {
		return fmt.Errorf("derived fork requires settled session recovery and terminal commits; inspect and reconcile the source first")
	}
	for _, item := range session.Tasks {
		if err := validateDerivedForkTaskSafety(item); err != nil {
			return err
		}
	}
	return nil
}

func validateDerivedForkTaskSafety(item *TodoItem) error {
	if item == nil || item.CheckpointPause {
		return fmt.Errorf("derived fork cannot discard a paused or unavailable task")
	}
	switch item.SideEffect {
	case SideEffectNone, SideEffectWorkspaceWrite:
		return nil
	case "", SideEffectUnknown, SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential:
		if item.RecoveryState == RecoveryStateNotStarted || item.RecoveryState == RecoveryStateComplete {
			return nil
		}
		// A never-started pending contract has no admitted execution to
		// discard. Every other uncertain external effect fails closed.
		if (item.Status == TaskPending || item.Status == TaskPlanned) && item.StartedAt.IsZero() && item.ExecutionReceipt == nil &&
			len(item.ExecutionReceipts) == 0 && item.RecoveryState == "" {
			return nil
		}
	default:
		return fmt.Errorf("derived fork task %s has an unsupported side-effect class", item.ID)
	}
	return fmt.Errorf("derived fork task %s has unconfirmed external effects; inspect and reconcile it before forking", item.ID)
}

// Attempt worlds are global to the control workspace. A branch-local replay
// cannot hide another branch's unfinished mutation of the shared subject.
func validateDerivedForkWorldSafety(events []RunEvent) error {
	worlds := attemptWorldRecordsFromEvents(events)
	completed := derivedForkCompletedWorlds(events, worlds)
	for _, world := range worlds {
		switch world.State {
		case attemptWorldStateApplying:
			return fmt.Errorf("derived fork requires isolated apply %s to settle first", world.WorldID)
		case attemptWorldStateApplied:
			if !completed[world.WorldID] {
				return fmt.Errorf("derived fork requires applied world %s to complete first", world.WorldID)
			}
		}
	}
	return nil
}

func derivedForkCompletedWorlds(events []RunEvent, worlds map[string]*attemptWorldRecord) map[string]bool {
	completed := make(map[string]bool)
	for _, event := range events {
		if event.Type == string(EventAttemptWorkspacePrepared) || event.Type == string(EventAttemptWorkspaceApplyStarted) || event.Type == string(EventAttemptWorkspaceApplied) {
			var payload attemptWorldEventPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				delete(completed, payload.WorldID)
			}
			continue
		}
		if event.Type != string(EventTaskCompleted) {
			continue
		}
		type worldReceipt struct {
			WorldID string `json:"execution_world_id"`
		}
		var payload struct {
			Status   TaskStatus     `json:"status"`
			Receipt  *worldReceipt  `json:"execution_receipt"`
			Receipts []worldReceipt `json:"execution_receipts"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.Status != TaskDone {
			continue
		}
		if payload.Receipt != nil {
			payload.Receipts = append(payload.Receipts, *payload.Receipt)
		}
		for _, receipt := range payload.Receipts {
			world := worlds[receipt.WorldID]
			if world != nil && event.TaskID == world.TaskID && effectiveEventBranchID(event) == effectiveBranchID(world.BranchID) {
				completed[receipt.WorldID] = true
			}
		}
	}
	return completed
}
