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
// Validated local completion receives zero additional model turns.
// The grant is committed in the task transition before a repair lease opens.
type ResultRepairGrant struct {
	RunID              string                              `json:"run_id"`
	BranchID           string                              `json:"branch_id"`
	AfterAttempts      int                                 `json:"after_attempts"`
	AdditionalAttempts int                                 `json:"additional_attempts"`
	TranscriptRef      string                              `json:"transcript_ref"`
	Model              string                              `json:"model,omitempty"`
	Recovery           *ResultCollectionRecoveryAssessment `json:"recovery,omitempty"`
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
		if !validArtifactID(item.ExecutionReceipt.TranscriptRef) {
			return errResultCollectionStale
		}
		reason := checkpoint.FailureReason
		if checkpoint.State != ResultCollectionBlocked {
			// Cancellation or a failed collection-event append can leave an
			// earlier durable state. Only the independently persisted native
			// failure receipt may authorize continuation of those bytes.
			reason = item.ExecutionReceipt.RepairProvenance.FailureReason
			if reason != repairFailureCollectionUnavailable && reason != RepairFailureCommit {
				return errResultCollectionStale
			}
		}
		switch reason {
		case RepairFailureInvalidSchema, RepairFailureResultMissing, repairFailurePreparation, repairFailureCollectionUnavailable, repairFailureCollectionBudget:
			return nil
		case RepairFailureCommit:
			return nil
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
	if item == nil || item.ExecutionReceipt == nil {
		return fmt.Errorf("task %q has no repair receipt", taskID)
	}
	if item.ExecutionReceipt.ResultCollection == nil {
		if err := validateResultRepairTarget(item); err != nil {
			return fmt.Errorf("task %q: %w", taskID, err)
		}
	}
	if item.ExecutionReceipt.RepairProvenance == nil {
		return fmt.Errorf("task %q has no failed submission", taskID)
	}
	for _, prior := range item.ExecutionReceipt.RepairProvenance.OperatorGrants {
		if prior.RunID == c.executionRunID {
			return fmt.Errorf("task %q already consumed this invocation's operator repair grant", taskID)
		}
	}
	if checkpoint := item.ExecutionReceipt.ResultCollection; checkpoint != nil {
		assessment, err := c.AssessResultCollectionRecovery(ctx, taskID)
		if err != nil {
			return err
		}
		if err := validateRecoveryAssessment(assessment); err != nil {
			return fmt.Errorf("task %q recovery blocked: %v: %w", taskID, assessment.Checks, err)
		}

		if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
			return fmt.Errorf("collection repair must retain its original target")
		}
		grant := &ResultRepairGrant{RunID: c.executionRunID, BranchID: c.activeBranchID(), AfterAttempts: checkpoint.TurnsUsed, AdditionalAttempts: assessment.AdditionalTurns, TranscriptRef: item.ExecutionReceipt.TranscriptRef, Recovery: assessment}
		return c.CommitTaskTransition(ctx, taskID, item.Status, TaskProtocolIncomplete,
			fmt.Sprintf("operator authorized %s after %s; at most %d additional collector turns; worker replay forbidden", assessment.Disposition, assessment.OriginalReason, assessment.AdditionalTurns), "", map[string]any{"result_repair_grant": grant})
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
	if grant != nil && grant.Recovery != nil {
		if validateRecoveryAssessment(grant.Recovery) != nil || next != TaskProtocolIncomplete || validateCollectionRecoveryTarget(item) != nil || grant.RunID == "" || grant.RunID != c.executionRunID || grant.BranchID != c.activeBranchID() || grant.Model != "" {
			return false
		}
		fresh, err := c.AssessResultCollectionRecovery(context.Background(), item.ID)
		if err != nil || validateRecoveryAssessment(fresh) != nil || fresh.Fingerprint != grant.Recovery.Fingerprint {
			return false
		}
		return grant.AfterAttempts == fresh.TurnsUsed && grant.AdditionalAttempts == fresh.AdditionalTurns && grant.TranscriptRef == fresh.TranscriptRef
	}
	if grant == nil || validateResultRepairTarget(item) != nil || next != TaskProtocolIncomplete {
		return false
	}
	if grant.RunID == "" || grant.RunID != c.executionRunID || grant.BranchID != c.activeBranchID() || grant.AfterAttempts != item.ExecutionReceipt.RepairProvenance.RepairAttempts || grant.TranscriptRef != item.ExecutionReceipt.TranscriptRef {
		return false
	}
	if item.ExecutionReceipt.ResultCollection != nil {
		// New collection grants always carry a freshly verified assessment.
		// Old grants remain readable through replay, not newly issuable here.
		return false
	}
	if item.ResultCandidate != nil && item.ResultCandidate.Prepared {
		return grant.AdditionalAttempts == 0 && grant.Model == ""
	}
	return grant.AdditionalAttempts == 2
}
