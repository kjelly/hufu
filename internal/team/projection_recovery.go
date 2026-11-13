package team

import (
	"encoding/json/v2"
	"strings"
)

// Keep a newly observed failure's fallback diagnostics, but explicitly persist
// a cleared active stop on non-failure transitions and authorized result repair.
func projectTaskActiveFailure(payload map[string]any, item *TodoItem, failureTransition bool) {
	if !failureTransition || item.FailureEvent != nil ||
		(item.Status == TaskProtocolIncomplete && operatorResultRepairGranted(item.ExecutionReceipt)) {
		payload["failure_event"] = RedactedFailureEvent(item.FailureEvent)
	}
}

func operatorResultRepairGranted(receipt *ExecutionReceipt) bool {
	return receipt != nil && receipt.RepairProvenance != nil &&
		len(receipt.RepairProvenance.OperatorGrants) > 0
}

func completedOperatorResultRepair(receipt *ExecutionReceipt) bool {
	return operatorResultRepairGranted(receipt) && receipt.ResultCollection != nil &&
		receipt.ResultCollection.State == ResultCollectionCommitted
}

// A corrected reducer can resolve a previously recorded projection mismatch.
// Clear only that exact, durably recorded barrier after full validated parity;
// unrelated recovery barriers and invalid canonical projections remain closed.
func (c *Coordinator) clearMatchedProjectionRecovery(lineage []RunEvent, replayed *SessionData) {
	if replayed == nil || replayed.RecoveryRequired {
		return
	}
	_ = c.mutateSessionData(func(sd *SessionData) error {
		if isRecordedProjectionRecovery(sd, replayed, lineage) {
			sd.RecoveryRequired = false
			sd.RecoveryReason = ""
		}
		return nil
	})
}

// Call only after complete canonical parity has been established. Read-only
// admission uses the same proof without clearing or persisting the barrier.
func isRecordedProjectionRecovery(checkpoint, replayed *SessionData, lineage []RunEvent) bool {
	if checkpoint == nil || replayed == nil || replayed.RecoveryRequired ||
		!checkpoint.RecoveryRequired || checkpoint.PendingTerminalCommit != nil ||
		!strings.HasPrefix(checkpoint.RecoveryReason, "event-store projection mismatch: ") {
		return false
	}
	for i := len(lineage) - 1; i >= 0; i-- {
		if lineage[i].Type == "projection_mismatch" {
			var payload struct {
				Reason string `json:"reason"`
			}
			return json.Unmarshal(lineage[i].Payload, &payload) == nil && payload.Reason == checkpoint.RecoveryReason
		}
	}
	return false
}
