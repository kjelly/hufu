package team

import (
	"context"
	"encoding/json/v2"
)

// recoverAbortedPreflightTerminal settles only a run canonically recorded as
// aborted before any execution. It cannot infer success, execute work, or
// discard an uncertain terminal append after model/tool activity.
func (c *Coordinator) recoverAbortedPreflightTerminal(lineage []RunEvent, pending *PendingTerminalCommit) (RunEvent, bool, error) {
	if pending.IdempotencyKey != terminalFinishedIdempotencyKey(pending.RunID) || pending.BranchID != c.activeBranchID() {
		return RunEvent{}, false, nil
	}
	started, aborted := false, false
	for _, event := range lineage {
		if event.RunID != pending.RunID {
			continue
		}
		if event.BranchID != pending.BranchID {
			return RunEvent{}, false, nil
		}
		switch event.Type {
		case "projection_rebuilt", "reliability_eval":
		case "run_started":
			if started || aborted {
				return RunEvent{}, false, nil
			}
			started = true
		case "coordinator_continuation_checkpoint":
			var checkpoint ContinuationCheckpoint
			if json.Unmarshal(event.Payload, &checkpoint) != nil || !started || checkpoint.Status != "aborted" || checkpoint.TurnCount != 0 {
				return RunEvent{}, false, nil
			}
			aborted = true
		default:
			return RunEvent{}, false, nil
		}
	}
	if !started || !aborted {
		return RunEvent{}, false, nil
	}
	result := &RunResult{RunID: pending.RunID, Outcome: RunOutcomeFailed, StopReason: StopReasonRunFailed, ExitCode: 1,
		Reason:   "recovered aborted invocation before execution",
		Response: "The invocation aborted before execution. Existing session tasks and evidence are retained.",
		Warnings: []string{"The original terminal append was not committed; the aborted preflight checkpoint was recovered as failed."}}
	payload, err := json.Marshal(terminalLifecyclePayload(c, result))
	if err != nil {
		return RunEvent{}, false, err
	}
	ctx, cancel := terminalFinalizationContext(context.Background())
	defer cancel()
	event, err := c.eventStore.AppendPersistedBoundedContext(ctx, RunEvent{
		Type: "run_finished", Actor: "coordinator", RunID: pending.RunID, BranchID: pending.BranchID,
		IdempotencyKey: pending.IdempotencyKey, Payload: payload,
	})
	return event, err == nil, err
}
