package team

import (
	"context"
	"errors"

	"github.com/kjelly/hufu/internal/agent"
)

func (c *Coordinator) validateRecoveryCanonicalClaims(ctx context.Context, cp *ResultCollectionCheckpoint, claims []byte, issue *agent.StructuredResponseIssue) error {
	input, err := c.decodeCollectionClaims(cp.TaskID, claims)
	if issue != nil {
		err = rejectLocalResult(RepairFailureInvalidSchema, errors.New(issue.Diagnostic()))
	}
	if err == nil {
		input, err = c.projectResultClaims(ctx, cp.TaskID, input)
		if err != nil {
			err = rejectProjectedClaims(err)
		}
	}
	if err == nil {
		admission := localResultAdmission{coordinator: c, todoID: cp.TaskID}
		var res *TaskResult
		res, err = admission.validateClaims(ctx, input, localResultSubmissionContract(c, cp.TaskID), func() RepairFailureReason { return RepairFailureInvalidSchema }, false)
		if err == nil {
			err = c.attestInvariantClaims(cp.TaskID, cp.Attempt, cp.ModelExecutionID, input.InvariantAssessments, res)
			if _, mismatch := errors.AsType[*invariantFindingSeverityMismatch](err); mismatch {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
		if err == nil {
			err = c.validateAcceptedRepairClaims(ctx, cp, res)
		}
		if err == nil {
			contract := localResultSubmissionContract(c, cp.TaskID)
			err = contract.validateFinalizableResult(res)
			if err != nil && collectionProjectionFormatFailure(contract, res) {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
	}
	return err
}
