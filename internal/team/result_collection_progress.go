package team

import (
	"context"
	"fmt"
)

func collectionReceipt(item *TodoItem, checkpoint *ResultCollectionCheckpoint) *ExecutionReceipt {
	if item == nil || checkpoint == nil {
		return nil
	}
	for index := range item.ExecutionReceipts {
		receipt := &item.ExecutionReceipts[index]
		if receipt.RunID == checkpoint.SourceRunID && receipt.Attempt == checkpoint.Attempt && receipt.ModelExecutionID == checkpoint.ModelExecutionID &&
			receipt.ResultCollection != nil && receipt.ResultCollection.Work == checkpoint.Work {
			return receipt
		}
	}
	if receipt := item.ExecutionReceipt; receipt != nil && receipt.RunID == checkpoint.SourceRunID && receipt.Attempt == checkpoint.Attempt && receipt.ModelExecutionID == checkpoint.ModelExecutionID &&
		receipt.ResultCollection != nil && receipt.ResultCollection.Work == checkpoint.Work {
		return receipt
	}
	return nil
}

func collectionTurnLimit(receipt *ExecutionReceipt) (int, error) {
	limit := resultCollectionDefaultTurns
	if receipt.RepairProvenance == nil {
		return limit, nil
	}
	checkpoint := receipt.ResultCollection
	for _, grant := range receipt.RepairProvenance.OperatorGrants {
		if checkpoint == nil || grant.RunID == "" || grant.BranchID != checkpoint.BranchID || grant.TranscriptRef != receipt.TranscriptRef ||
			grant.Model != "" || grant.AfterAttempts < 0 || grant.AfterAttempts > checkpoint.TurnsUsed ||
			(grant.AdditionalAttempts != 0 && grant.AdditionalAttempts != 2) {
			return 0, errResultCollectionStale
		}
		if grant.Recovery != nil {
			if err := validateStoredRecoveryGrant(grant, checkpoint); err != nil {
				return 0, err
			}
		}
		limit = max(limit, grant.AfterAttempts+grant.AdditionalAttempts)
	}
	return limit, nil
}

// updateResultCollection serializes checkpoint changes with lifecycle changes.
// The pinned checkpoint must still be current, and an active runtime lease is
// required before reserving another model turn or publishing its response.
func (c *Coordinator) updateResultCollection(ctx context.Context, expected *ResultCollectionCheckpoint, update func(*ResultCollectionCheckpoint, *ExecutionReceipt) error) (*ResultCollectionCheckpoint, error) {
	if expected == nil {
		return nil, errResultCollectionStale
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	controller := c.occurrenceController(expected.TaskID)
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	item := c.todoItemByID(expected.TaskID)
	receipt := collectionReceipt(item, expected)
	if receipt == nil {
		return nil, errResultCollectionStale
	}
	receiptCopy := cloneExecutionReceipt(receipt)
	receipt = &receiptCopy
	current := receipt.ResultCollection
	pinned, err := collectionDigest(expected)
	if err != nil {
		return nil, err
	}
	actual, err := collectionDigest(current)
	if err != nil || actual != pinned {
		return nil, errResultCollectionStale
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, expected.TaskID)
	if err != nil || identity.RunID != expected.SourceRunID || identity.Attempt != expected.Attempt || identity.Agent != expected.Agent ||
		identity.OccurrenceRevision != expected.SubmissionRevision || identity.DispatchID != expected.SubmissionDispatchID {
		return nil, errResultCollectionStale
	}
	snapshot := controller.snapshot()
	if !snapshot.opened || snapshot.reserved || snapshot.result != nil || !sameTaskResultOccurrence(snapshot.identity, identity) {
		return nil, errTaskResultStale
	}
	if _, err := c.loadCollectionWork(ctx, item, expected); err != nil {
		return nil, err
	}
	limit, err := collectionTurnLimit(receipt)
	if err != nil || limit != expected.TurnLimit {
		return nil, errResultCollectionStale
	}
	next := cloneResultCollectionCheckpoint(current)
	next.Revision++
	if err := update(next, receipt); err != nil {
		return nil, err
	}
	if err := c.appendCollectionCheckpointLocked(ctx, item, receipt, next, nil); err != nil {
		return nil, err
	}
	return cloneResultCollectionCheckpoint(next), nil
}

func (c *Coordinator) reserveResultCollection(ctx context.Context, expected *ResultCollectionCheckpoint, invocationID string, request []byte) (*ResultCollectionCheckpoint, error) {
	if invocationID == "" || c.executionRunID == "" || len(request) == 0 {
		return nil, fmt.Errorf("result collection requires a frozen request and invocation identity")
	}
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, receipt *ExecutionReceipt) error {
		if next.State == ResultCollectionPrepared || next.State == ResultCollectionCommitted {
			return errResultCollectionStale
		}
		if next.State == ResultCollectionReceived && next.FailureReason != RepairFailureInvalidSchema {
			return errResultCollectionStale
		}
		if next.State == ResultCollectionBlocked {
			if !collectionGrantedForRun(receipt, c.executionRunID) || next.Prepared != nil {
				return errResultCollectionBlocked
			}
		}
		if next.State == ResultCollectionInFlight {
			resuming, _ := ctx.Value(sessionResumeKey{}).(bool)
			if !resuming || next.InvocationRunID == c.executionRunID {
				return errResultCollectionStale
			}
		}
		if next.TurnsUsed >= next.TurnLimit {
			return errResultCollectionBudget
		}
		next.State, next.InvocationID = ResultCollectionInFlight, invocationID
		next.InvocationRunID = c.executionRunID
		next.TurnsUsed++
		next.Response, next.Prepared, next.Accepted = nil, nil, nil
		next.FailureReason = ""
		ref, err := c.putCollectionBlob(ctx, next, "request", request)
		if err != nil {
			return err
		}
		next.Request = ref
		return nil
	})
}

func (c *Coordinator) recordCollectionSchemaFailure(ctx context.Context, expected *ResultCollectionCheckpoint) (*ResultCollectionCheckpoint, error) {
	if expected == nil {
		return nil, errResultCollectionStale
	}
	raw, err := c.readCollectionBlob(ctx, expected, "response", expected.Response)
	if err != nil {
		return nil, err
	}
	_, rejection := c.decodeCollectionClaims(expected.TaskID, raw)
	if reason := localResultRejectionReason(rejection); rejection == nil || (reason != RepairFailureInvalidSchema && reason != RepairFailureResultMissing) {
		return nil, errResultCollectionStale
	}
	return c.recordCollectionFormatFailure(ctx, expected)
}

// Called only after decoding or canonical admission rejects model formatting.
// The original response remains durable; a new invocation consumes the same
// bounded budget and never reopens worker execution.
func (c *Coordinator) recordCollectionFormatFailure(ctx context.Context, expected *ResultCollectionCheckpoint) (*ResultCollectionCheckpoint, error) {
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, _ *ExecutionReceipt) error {
		if next.State != ResultCollectionReceived && next.State != ResultCollectionPrepared {
			return errResultCollectionStale
		}
		next.State, next.Prepared = ResultCollectionReceived, nil
		next.FailureReason = RepairFailureInvalidSchema
		return nil
	})
}

func (c *Coordinator) recordCollectionResponse(ctx context.Context, expected *ResultCollectionCheckpoint, response []byte) (*ResultCollectionCheckpoint, error) {
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, _ *ExecutionReceipt) error {
		if next.State != ResultCollectionInFlight || next.InvocationRunID != c.executionRunID {
			return errResultCollectionStale
		}
		ref, err := c.putCollectionBlob(ctx, next, "response", response)
		if err != nil {
			return err
		}
		next.State, next.Response = ResultCollectionReceived, ref
		return nil
	})
}

func (c *Coordinator) blockResultCollection(ctx context.Context, expected *ResultCollectionCheckpoint, reason RepairFailureReason) (*ResultCollectionCheckpoint, error) {
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, receipt *ExecutionReceipt) error {
		if reason == "" || next.State == ResultCollectionCommitted {
			return errResultCollectionStale
		}
		next.State, next.FailureReason = ResultCollectionBlocked, reason
		if receipt.RepairProvenance == nil {
			receipt.RepairProvenance = &RepairProvenance{}
		}
		receipt.RepairProvenance.Attempted = true
		receipt.RepairProvenance.RepairAttempts = next.TurnsUsed
		receipt.RepairProvenance.FailureReason = reason
		return nil
	})
}

func collectionGrantedForRun(receipt *ExecutionReceipt, runID string) bool {
	if receipt == nil || receipt.RepairProvenance == nil {
		return false
	}
	for _, grant := range receipt.RepairProvenance.OperatorGrants {
		if grant.RunID == runID {
			return true
		}
	}
	return false
}
