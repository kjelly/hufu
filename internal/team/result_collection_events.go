package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

func validateResultCollectionEvent(event RunEvent) error {
	var payload resultCollectionEvent
	if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	checkpoint := payload.Receipt.ResultCollection
	if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
		return err
	}
	if event.TaskID != checkpoint.TaskID || effectiveBranchID(event.BranchID) != checkpoint.BranchID || event.Attempt != checkpoint.Attempt ||
		payload.Receipt.RunID != checkpoint.SourceRunID || payload.Receipt.TaskID != checkpoint.TaskID || payload.Receipt.Attempt != checkpoint.Attempt ||
		payload.Receipt.ModelExecutionID != checkpoint.ModelExecutionID || payload.Receipt.ExecutionTarget != checkpoint.ExecutionTarget {
		return fmt.Errorf("result collection event identity mismatch")
	}
	if (checkpoint.State == ResultCollectionCommitted) != (payload.Result != nil) {
		return fmt.Errorf("result collection event has invalid accepted-result projection")
	}
	limit, err := collectionTurnLimit(&payload.Receipt)
	if err != nil || limit != checkpoint.TurnLimit {
		return fmt.Errorf("result collection event has an ungranted model budget")
	}
	if payload.Result != nil && (payload.Result.TaskID != checkpoint.TaskID || payload.Result.Attempt != checkpoint.Attempt || payload.Result.Agent != checkpoint.Agent || !isLocalSubmittedResultSource(payload.Result.Source)) {
		return fmt.Errorf("result collection event has invalid canonical result identity")
	}
	if payload.Result != nil && payload.Result.StructuredPayload != nil {
		resultPayload := payload.Result.StructuredPayload
		if payload.Receipt.ResultValidation != ResultValidationValid || payload.Receipt.ResultPayloadSHA256 != resultPayload.SHA256 || payload.Receipt.ResultContractID != resultPayload.Contract.ID || payload.Receipt.ResultContractSHA256 != resultPayload.Contract.SchemaSHA256 {
			return fmt.Errorf("result collection event has invalid result contract receipt binding")
		}
	}
	return nil
}

// appendCollectionCheckpointLocked is called with both the task projection and
// occurrence projection locks held. Publication follows the durable event; the
// returned event is compared because Append may acknowledge a competing writer's
// idempotent operation instead of the supplied payload.
func (c *Coordinator) appendCollectionCheckpointLocked(ctx context.Context, item *TodoItem, original *ExecutionReceipt, checkpoint *ResultCollectionCheckpoint, result *TaskResult) error {
	if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
		return err
	}
	prior := collectionForReceipt(item, original)
	if prior == nil {
		if checkpoint.Revision != 1 || checkpoint.State != ResultCollectionPending {
			return errResultCollectionStale
		}
	} else {
		before, err := collectionBindingDigest(prior)
		if err != nil {
			return err
		}
		after, err := collectionBindingDigest(checkpoint)
		if err != nil || before != after || checkpoint.Work != prior.Work || checkpoint.Revision != prior.Revision+1 || checkpoint.TurnsUsed < prior.TurnsUsed {
			return errResultCollectionStale
		}
		if err := validateCollectionTransition(prior, checkpoint); err != nil {
			return err
		}
	}
	receipt := cloneExecutionReceipt(original)
	receipt.ResultCollection = cloneResultCollectionCheckpoint(checkpoint)
	if result != nil {
		c.applyResultContractReceipt(checkpoint.TaskID, result, &receipt)
	}
	payload := resultCollectionEvent{Receipt: receipt, Result: cloneTaskResult(result)}
	body, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return err
	}
	event := RunEvent{Type: string(EventTaskResultCollectionCheckpointed), RunID: c.executionRunID, BranchID: checkpoint.BranchID, Actor: checkpoint.Agent,
		TaskID: checkpoint.TaskID, Attempt: checkpoint.Attempt, Payload: body,
		IdempotencyKey: fmt.Sprintf("result-collection:%s:%s:%s:%d:%s:%s:%d", checkpoint.BranchID, checkpoint.SourceRunID, checkpoint.TaskID, checkpoint.Attempt, checkpoint.WorkDispatchID, checkpoint.ModelExecutionID, checkpoint.Revision)}
	if err := validateResultCollectionEvent(event); err != nil {
		return err
	}
	durable, err := c.EventJournal().Append(ctx, event)
	if err != nil {
		return err
	}
	var accepted resultCollectionEvent
	if err := json.Unmarshal(durable.Payload, &accepted); err != nil {
		return err
	}
	expected, err := collectionDigest(payload)
	if err != nil {
		return err
	}
	actual, err := collectionDigest(accepted)
	if err != nil || expected != actual {
		return errResultCollectionStale
	}
	projected := cloneTodoItem(item)
	applyResultCollectionEvent(projected, accepted)
	return c.taskTracker.TodoList().TryApplyProjectedItem(projected)
}

func applyResultCollectionEvent(item *TodoItem, payload resultCollectionEvent) {
	if item == nil {
		return
	}
	receipt := cloneExecutionReceipt(&payload.Receipt)
	item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, receipt)
	if item.ExecutionReceipt == nil || sameExecutionReceiptProjection(*item.ExecutionReceipt, receipt) {
		item.ExecutionReceipt = &receipt
	}
	// Dispatch activation precedes the work checkpoint but is not a separate
	// lifecycle event. The checkpoint is its durable lease record; restore it
	// before attempting local admission after replay. Leaf leases remain private.
	if checkpoint := receipt.ResultCollection; !checkpoint.FanoutLeaf &&
		(item.Status == TaskInProgress || item.Status == TaskProtocolIncomplete) {
		item.OccurrenceRevision = checkpoint.SubmissionRevision
		item.DispatchID = checkpoint.SubmissionDispatchID
	}
	if payload.Result != nil && !receipt.ResultCollection.FanoutLeaf {
		item.TypedResult = cloneTaskResult(payload.Result)
	}
}

func validateCollectionTransition(prior, next *ResultCollectionCheckpoint) error {
	if next.TurnLimit < prior.TurnLimit || next.TurnsUsed > prior.TurnsUsed+1 {
		return errResultCollectionBudget
	}
	switch next.State {
	case ResultCollectionInFlight:
		if (prior.State != ResultCollectionPending && prior.State != ResultCollectionReceived && prior.State != ResultCollectionInFlight && prior.State != ResultCollectionBlocked) ||
			next.TurnsUsed != prior.TurnsUsed+1 || next.InvocationID == prior.InvocationID || next.Response != nil || next.Prepared != nil {
			return errResultCollectionStale
		}
	case ResultCollectionReceived:
		if prior.State != ResultCollectionInFlight && prior.State != ResultCollectionReceived {
			return errResultCollectionStale
		}
	case ResultCollectionPrepared:
		if prior.State != ResultCollectionReceived {
			return errResultCollectionStale
		}
	case ResultCollectionCommitted:
		if prior.State != ResultCollectionPrepared {
			return errResultCollectionStale
		}
	case ResultCollectionBlocked:
		if prior.State == ResultCollectionCommitted {
			return errResultCollectionStale
		}
	default:
		return errResultCollectionStale
	}
	if next.State != ResultCollectionInFlight && (next.TurnsUsed != prior.TurnsUsed || next.InvocationID != prior.InvocationID || next.InvocationRunID != prior.InvocationRunID) {
		return errResultCollectionStale
	}
	return nil
}
