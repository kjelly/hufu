package team

import (
	"context"
	"fmt"
)

type resultOnlyRepairKey struct{}

// WithResultOnlyRepair explicitly authorizes one bounded repair cycle for an
// existing protocol failure. It never authorizes worker execution or browsing.
func WithResultOnlyRepair(ctx context.Context, taskID string) context.Context {
	return context.WithValue(WithSessionResume(ctx), resultOnlyRepairKey{}, taskID)
}

// ResultRepairGrant extends submission repair, preserving every previous turn.
// The grant is committed in the task transition before a repair lease opens.
type ResultRepairGrant struct {
	RunID              string `json:"run_id"`
	BranchID           string `json:"branch_id"`
	AfterAttempts      int    `json:"after_attempts"`
	AdditionalAttempts int    `json:"additional_attempts"`
	TranscriptRef      string `json:"transcript_ref"`
	Model              string `json:"model,omitempty"`
}

func validateResultRepairTarget(item *TodoItem) error {
	if item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureProtocol {
		return fmt.Errorf("result-only repair requires an existing blocked protocol task")
	}
	if item.WorkerWorkspace.isolated() {
		return fmt.Errorf("operator result-only repair does not support isolated workspaces")
	}
	if item.ExecutionReceipt == nil || item.ExecutionReceipt.RepairProvenance == nil || !item.ExecutionReceipt.RepairProvenance.Attempted || item.ExecutionReceipt.RepairProvenance.Success {
		return fmt.Errorf("result-only repair requires the original failed submission receipt")
	}
	if item.ResultContract == nil || !validArtifactID(item.ExecutionReceipt.TranscriptRef) {
		return fmt.Errorf("result-only repair requires a frozen result contract and original transcript")
	}
	reason := item.ExecutionReceipt.RepairProvenance.FailureReason
	if reason != RepairFailureNoToolCall && reason != RepairFailureInvalidSchema && reason != repairFailurePreparation {
		return fmt.Errorf("result-only repair cannot repair execution failure %q", reason)
	}
	return nil
}

func (c *Coordinator) authorizeOperatorResultRepair(ctx context.Context) error {
	taskID, _ := ctx.Value(resultOnlyRepairKey{}).(string)
	if taskID == "" {
		return nil
	}
	item := c.todoItemByID(taskID)
	if err := validateResultRepairTarget(item); err != nil {
		return fmt.Errorf("task %q: %w", taskID, err)
	}
	for _, prior := range item.ExecutionReceipt.RepairProvenance.OperatorGrants {
		if prior.RunID == c.executionRunID {
			return fmt.Errorf("task %q already consumed this invocation's operator repair grant", taskID)
		}
	}
	if _, err := c.restoreProtocolRepairToolEvidence(ctx, item); err != nil {
		return fmt.Errorf("task %q evidence: %w", taskID, err)
	}
	grant := &ResultRepairGrant{
		RunID: c.executionRunID, BranchID: c.activeBranchID(),
		AfterAttempts:      item.ExecutionReceipt.RepairProvenance.RepairAttempts,
		AdditionalAttempts: 2, TranscriptRef: item.ExecutionReceipt.TranscriptRef,
	}
	if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
		if _, err := c.resultRepairTarget(model); err != nil {
			return err
		}
		grant.Model = model
	}
	return c.CommitTaskTransition(ctx, taskID, item.Status, TaskProtocolIncomplete,
		"operator authorized bounded result-only repair; worker replay forbidden", "",
		map[string]any{"result_repair_grant": grant})
}

func (c *Coordinator) validResultRepairGrant(item *TodoItem, next TaskStatus, grant *ResultRepairGrant) bool {
	return grant != nil && validateResultRepairTarget(item) == nil && next == TaskProtocolIncomplete &&
		grant.RunID != "" && grant.RunID == c.executionRunID && grant.BranchID == c.activeBranchID() &&
		grant.AdditionalAttempts == 2 && grant.AfterAttempts == item.ExecutionReceipt.RepairProvenance.RepairAttempts &&
		grant.TranscriptRef == item.ExecutionReceipt.TranscriptRef
}
