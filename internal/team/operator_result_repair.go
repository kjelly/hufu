package team

import (
	"context"
	"fmt"
	"strings"
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
	RunID               string                              `json:"run_id"`
	BranchID            string                              `json:"branch_id"`
	AfterAttempts       int                                 `json:"after_attempts"`
	AdditionalAttempts  int                                 `json:"additional_attempts"`
	TranscriptRef       string                              `json:"transcript_ref"`
	Model               string                              `json:"model,omitempty"`
	ReferenceProjection *AcceptedReferenceProjection        `json:"reference_projection,omitempty"`
	Recovery            *ResultCollectionRecoveryAssessment `json:"recovery,omitempty"`
}

func validateResultRepairTarget(item *TodoItem) error {
	if item == nil || item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureProtocol {
		return fmt.Errorf("result-only repair requires an existing blocked protocol task")
	}
	if canResumeDirectNormalization(item) {
		return nil
	}
	if item.WorkerWorkspace.isolated() {
		return fmt.Errorf("operator result-only repair requires a sealed submission for isolated workspaces")
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
	if hasBoundProtocolSchemaRepair(item.ExecutionReceipt) && (item.ResultCandidate == nil || !item.ResultCandidate.Prepared) &&
		item.ExecutionReceipt.RepairProvenance.RepairAttempts >= protocolRepairLimit(item.ExecutionReceipt.WorkSubmissionFailure, protocolRepairHistory(item.ExecutionReceipt)) {
		return fmt.Errorf("result-only repair already consumed the schema correction")
	}
	return nil
}

func (c *Coordinator) authorizeOperatorResultRepair(ctx context.Context) error {
	taskID, _ := ctx.Value(resultOnlyRepairKey{}).(string)
	if taskID == "" {
		return nil
	}
	item := c.todoItemByID(taskID)
	if item != nil && len(selectedAcceptedReferences(ctx)) > 0 && acceptedRepairProof(currentResultCollection(item)) == nil {
		return fmt.Errorf("reference selection requires an accepted-result repair")
	}
	if item == nil || item.ExecutionReceipt == nil {
		return fmt.Errorf("task %q has no repair receipt", taskID)
	}
	if item.ExecutionReceipt.ResultCollection == nil {
		if tokens, _ := ctx.Value(resultRepairMaxOutputKey{}).(int); tokens != 0 {
			return fmt.Errorf("collector output limit requires a frozen collection checkpoint")
		}
		if err := validateResultRepairTarget(item); err != nil {
			return fmt.Errorf("task %q: %w", taskID, err)
		}
	}
	if item.ExecutionReceipt.RepairProvenance == nil && item.ExecutionReceipt.ResultCollection == nil {
		return fmt.Errorf("task %q has no failed submission", taskID)
	}
	for _, prior := range collectionRepairGrants(item.ExecutionReceipt) {
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
		grant := c.collectionResultRepairGrant(assessment)
		if assessment.Disposition == RecoverySelectReferences {
			grant.ReferenceProjection, err = c.prepareAcceptedReferenceProjection(ctx, assessment)
			if err != nil {
				return err
			}
		}
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
	if canResumeDirectNormalization(item) {
		if _, err := c.loadDirectNormalizationRecovery(ctx, item); err != nil {
			return err
		}
		grant.AdditionalAttempts = 0
	} else if item.ResultCandidate != nil && item.ResultCandidate.Prepared {
		if _, err := c.loadResultCandidate(ctx, item, item.ResultCandidate); err != nil {
			return err
		}
		grant.AdditionalAttempts = 0
	} else if hasBoundProtocolSchemaRepair(item.ExecutionReceipt) {
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

func (c *Coordinator) collectionResultRepairGrant(a *ResultCollectionRecoveryAssessment) *ResultRepairGrant {
	return &ResultRepairGrant{RunID: c.executionRunID, BranchID: c.activeBranchID(), AfterAttempts: a.TurnsUsed,
		AdditionalAttempts: a.AdditionalTurns, TranscriptRef: a.TranscriptRef, Recovery: a}
}

func (c *Coordinator) validateResultRepairGrant(ctx context.Context, item *TodoItem, next TaskStatus, grant *ResultRepairGrant) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if grant != nil && grant.Recovery != nil {
		if err := validateRecoveryAssessment(grant.Recovery); err != nil {
			return fmt.Errorf("recovery assessment proof: %w", err)
		}
		if next != TaskProtocolIncomplete || validateCollectionRecoveryTarget(item) != nil || grant.RunID == "" || grant.RunID != c.executionRunID || grant.BranchID != c.activeBranchID() || grant.Model != "" {
			return fmt.Errorf("recovery grant identity: %w", errResultCollectionStale)
		}
		fresh, err := c.AssessResultCollectionRecovery(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("recovery grant reassessment: %w", err)
		}
		if err := validateRecoveryAssessment(fresh); err != nil {
			return fmt.Errorf("recovery grant reassessment %s: %w", recoveryGrantDiagnostic(fresh), err)
		}
		if fresh.Fingerprint != grant.Recovery.Fingerprint {
			return fmt.Errorf("recovery grant assessment changed: before=%s after=%s: %w", recoveryGrantDiagnostic(grant.Recovery), recoveryGrantDiagnostic(fresh), errResultCollectionStale)
		}
		if grant.AfterAttempts != fresh.TurnsUsed || grant.AdditionalAttempts != fresh.AdditionalTurns || grant.TranscriptRef != fresh.TranscriptRef {
			return fmt.Errorf("recovery grant budget or transcript changed: %w", errResultCollectionStale)
		}
		if fresh.Disposition == RecoverySelectReferences {
			preview, _ := ctx.Value(referenceProjectionPreviewKey{}).(bool)
			if grant.ReferenceProjection == nil {
				if preview {
					return nil
				}
				return errResultCollectionStale
			}
			if !strings.HasPrefix(grant.ReferenceProjection.InvocationID, "operator-reference-") {
				return errResultCollectionStale
			}
			if err := c.validateAcceptedReferenceProjectionBlobs(ctx, grant); err != nil {
				return err
			}
		} else if fresh.Disposition != RecoverySelectReferences && grant.ReferenceProjection != nil {
			return errResultCollectionStale
		}
		return nil
	}
	if grant == nil || validateResultRepairTarget(item) != nil || next != TaskProtocolIncomplete {
		return fmt.Errorf("invalid result repair target: %w", errResultCollectionStale)
	}
	if grant.RunID == "" || grant.RunID != c.executionRunID || grant.BranchID != c.activeBranchID() || grant.AfterAttempts != item.ExecutionReceipt.RepairProvenance.RepairAttempts || grant.TranscriptRef != item.ExecutionReceipt.TranscriptRef {
		return fmt.Errorf("result repair grant identity: %w", errResultCollectionStale)
	}
	if item.ExecutionReceipt.ResultCollection != nil {
		return fmt.Errorf("collection grant requires a verified assessment: %w", errResultCollectionStale)
	}
	if canResumeDirectNormalization(item) || item.ResultCandidate != nil && item.ResultCandidate.Prepared || hasBoundProtocolSchemaRepair(item.ExecutionReceipt) {
		if grant.AdditionalAttempts == 0 && grant.Model == "" {
			return nil
		}
	} else if grant.AdditionalAttempts == 2 {
		return nil
	}
	return fmt.Errorf("result repair grant budget changed: %w", errResultCollectionStale)
}
