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

// ResultRepairGrant authorizes submission repair, preserving every previous turn.
// Prepared candidates receive zero additional model turns for local completion.
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
	if checkpoint := item.ExecutionReceipt.ResultCollection; checkpoint != nil {
		if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
			return err
		}
		if checkpoint.State != ResultCollectionBlocked || !validArtifactID(item.ExecutionReceipt.TranscriptRef) {
			return errResultCollectionStale
		}
		switch checkpoint.FailureReason {
		case RepairFailureInvalidSchema, repairFailurePreparation, repairFailureCollectionUnavailable, repairFailureCollectionBudget:
			return nil
		case RepairFailureCommit:
			if checkpoint.Prepared != nil {
				return nil
			}
		}
		return fmt.Errorf("result-only repair cannot bypass collection failure %q", checkpoint.FailureReason)
	}
	if item.ResultContract == nil || !validArtifactID(item.ExecutionReceipt.TranscriptRef) {
		return fmt.Errorf("result-only repair requires a frozen result contract and original transcript")
	}
	reason := item.ExecutionReceipt.RepairProvenance.FailureReason
	if reason != RepairFailureNoToolCall && reason != RepairFailureInvalidSchema && reason != repairFailurePreparation &&
		(reason != RepairFailureCommit || item.ResultCandidate == nil || !item.ResultCandidate.Prepared) {
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
	if checkpoint := item.ExecutionReceipt.ResultCollection; checkpoint != nil {
		if _, err := c.loadCollectionWork(ctx, item, checkpoint); err != nil {
			return err
		}
		if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
			return fmt.Errorf("collection repair must retain its original target")
		}
		grant := &ResultRepairGrant{RunID: c.executionRunID, BranchID: c.activeBranchID(), AfterAttempts: checkpoint.TurnsUsed, AdditionalAttempts: 2, TranscriptRef: item.ExecutionReceipt.TranscriptRef}
		if checkpoint.Prepared != nil {
			grant.AdditionalAttempts = 0
		}
		return c.CommitTaskTransition(ctx, taskID, item.Status, TaskProtocolIncomplete,
			"operator authorized collection continuation; worker replay forbidden", "", map[string]any{"result_repair_grant": grant})
	}
	if _, err := c.restoreProtocolRepairToolEvidence(ctx, item); err != nil {
		return fmt.Errorf("task %q evidence: %w", taskID, err)
	}
	grant := &ResultRepairGrant{
		RunID: c.executionRunID, BranchID: c.activeBranchID(),
		AfterAttempts:      item.ExecutionReceipt.RepairProvenance.RepairAttempts,
		AdditionalAttempts: 2, TranscriptRef: item.ExecutionReceipt.TranscriptRef,
	}
	if item.ResultCandidate != nil && item.ResultCandidate.Prepared {
		if _, err := c.loadResultCandidate(ctx, item, item.ResultCandidate); err != nil {
			return err
		}
		grant.AdditionalAttempts = 0
	} else if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
		if _, err := c.resultRepairTarget(model); err != nil {
			return err
		}
		grant.Model = model
	}
	return c.CommitTaskTransition(ctx, taskID, item.Status, TaskProtocolIncomplete,
		"operator authorized result-only continuation; worker replay forbidden", "",
		map[string]any{"result_repair_grant": grant})
}

func (c *Coordinator) validResultRepairGrant(item *TodoItem, next TaskStatus, grant *ResultRepairGrant) bool {
	if grant == nil || validateResultRepairTarget(item) != nil || next != TaskProtocolIncomplete {
		return false
	}
	if grant.RunID == "" || grant.RunID != c.executionRunID || grant.BranchID != c.activeBranchID() || grant.AfterAttempts != item.ExecutionReceipt.RepairProvenance.RepairAttempts || grant.TranscriptRef != item.ExecutionReceipt.TranscriptRef {
		return false
	}
	if checkpoint := item.ExecutionReceipt.ResultCollection; checkpoint != nil {
		additional := 2
		if checkpoint.Prepared != nil {
			additional = 0
		}
		return grant.AfterAttempts == checkpoint.TurnsUsed && grant.AdditionalAttempts == additional && grant.Model == ""
	}
	if item.ResultCandidate != nil && item.ResultCandidate.Prepared {
		return grant.AdditionalAttempts == 0 && grant.Model == ""
	}
	return grant.AdditionalAttempts == 2
}
