package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"

	"charm.land/fantasy"
)

type resultCollectionAdmissionKey struct{}

// Collection publishes only fields that the durable task can accept. Keep the
// legacy tool schema separate so persisted tool sessions retain compatibility.
func collectionSubmissionInfo(c *Coordinator, taskID string) fantasy.ToolInfo {
	info := localResultSubmissionInfo(c, taskID)
	if localResultSubmissionContract(c, taskID).InvariantVerification == "" {
		delete(info.Parameters, "invariant_assessments")
	}
	return info
}

// decodeCollectionClaims accepts exactly the published model claims schema.
// Only the schema's declared file-entry shorthand is normalized; text is never
// unwrapped and unsupported shorthand remains invalid.
func (c *Coordinator) decodeCollectionClaims(taskID string, raw []byte) (SubmitResultInput, error) {
	var input SubmitResultInput
	if len(bytes.TrimSpace(raw)) == 0 {
		return input, rejectLocalResult(RepairFailureResultMissing, fmt.Errorf("collector returned no result"))
	}
	if len(raw) > resultCollectionResponseMaxBytes || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return input, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("collection response must be one bounded JSON object"))
	}
	if _, err := decodeUniqueJSON(raw); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	info := collectionSubmissionInfo(c, taskID)
	if err := validateSubmissionFieldPresence(string(raw), info); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := validateToolArguments(string(raw), info); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := c.validateFixedCollectionInputJSON(taskID, string(raw)); err != nil {
		return input, rejectLocalResult(RepairFailureContract, err)
	}
	normalized, err := normalizeCollectionFileRefs(raw)
	if err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := json.Unmarshal(normalized, &input, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if input.Summary == "" {
		return input, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("summary is required"))
	}
	return input, nil
}

func (c *Coordinator) prepareCollectionClaims(ctx context.Context, expected *ResultCollectionCheckpoint) (*ResultCollectionCheckpoint, error) {
	raw, err := c.readCollectionBlob(ctx, expected, "response", expected.Response)
	if err != nil {
		return nil, err
	}
	if _, err := c.decodeCollectionClaims(expected.TaskID, raw); err != nil {
		return nil, err
	}
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, _ *ExecutionReceipt) error {
		if next.State != ResultCollectionReceived {
			return errResultCollectionStale
		}
		ref, err := c.putCollectionBlob(ctx, next, "claims", raw)
		if err != nil {
			return err
		}
		next.State, next.Prepared = ResultCollectionPrepared, ref
		return nil
	})
}

// commitPreparedCollection revalidates durable claims through ordinary local
// admission. It neither constructs a provider nor executes a tool handler.
func (c *Coordinator) commitPreparedCollection(ctx context.Context, expected *ResultCollectionCheckpoint) error {
	if expected == nil || expected.State != ResultCollectionPrepared {
		return errResultCollectionStale
	}
	var err error
	ctx, err = c.restoreCollectionContext(ctx, expected)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	item := c.todoItemByID(expected.TaskID)
	work, err := c.loadCollectionWork(ctx, item, expected)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	raw, err := c.readCollectionBlob(ctx, expected, "claims", expected.Prepared)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	if _, err := c.decodeCollectionClaims(item.ID, raw); err != nil {
		return err
	}
	// Resolve only keys issued by successful authorized views in the original
	// work occurrence. Canonical validation and storage still require full IDs.
	resolved, err := c.resolveResultEvidenceKeys(ctx, item.ID, string(raw))
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	input, err := c.decodeCollectionClaims(item.ID, []byte(resolved))
	if err != nil {
		return err
	}
	// A malformed model field must not conceal forged receipts or artifact
	// claims and earn a new model turn before those sources are checked.
	claims := input.taskResult()
	claims.Attempt = expected.Attempt
	if err := c.validateTaskResultReceiptClaims(item.ID, &claims); err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	if len(claims.Artifacts) > 0 {
		if err := validateSubmittedArtifactClaims(ctx, work.Workspace, claims.Artifacts); err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
	}
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, expected)
	admission := localResultAdmission{coordinator: c, todoID: item.ID}
	return admission.admit(ctx, input, localResultSubmissionContract(c, item.ID), func() RepairFailureReason {
		// validateStructuredPayloadClaims classifies canonical model-schema
		// failures after independent source checks. Other failures stay closed.
		return RepairFailureSource
	})
}

func (c *Coordinator) acceptedCollectionResult(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint) (*TaskResult, error) {
	if checkpoint == nil || checkpoint.State != ResultCollectionCommitted {
		return nil, errResultCollectionStale
	}
	if _, err := c.loadCollectionWork(ctx, item, checkpoint); err != nil {
		return nil, err
	}
	body, err := c.readCollectionBlob(ctx, checkpoint, "accepted", checkpoint.Accepted)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return nil, err
	}
	if result.TaskID != checkpoint.TaskID || result.Attempt != checkpoint.Attempt || result.Agent != checkpoint.Agent || !isLocalSubmittedResultSource(result.Source) {
		return nil, errResultCollectionStale
	}
	return &result, nil
}

// Persist the accepted canonical bytes before publishing the occurrence latch.
// Lifecycle transitions and competing submissions use the same projection lock.
func (tx *taskResultOccurrenceTransaction) commitCollection(ctx context.Context, result *TaskResult, expected *ResultCollectionCheckpoint) error {
	if tx == nil || tx.controller == nil || tx.finished || result == nil || expected == nil {
		return errTaskResultStale
	}
	if err := tx.validateCommit(result); err != nil {
		return err
	}
	c := tx.coordinator
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	tx.controller.projectionMu.Lock()
	defer tx.controller.projectionMu.Unlock()
	snapshot := tx.controller.snapshot()
	if !snapshot.opened || !snapshot.reserved || snapshot.result != nil || !sameTaskResultOccurrence(snapshot.identity, tx.identity) ||
		tx.identity.RunID != expected.SourceRunID || tx.identity.TaskID != expected.TaskID || tx.identity.Attempt != expected.Attempt ||
		tx.identity.OccurrenceRevision != expected.SubmissionRevision || tx.identity.DispatchID != expected.SubmissionDispatchID {
		return errTaskResultStale
	}
	item := c.todoItemByID(expected.TaskID)
	receipt := collectionReceipt(item, expected)
	if receipt == nil {
		return errResultCollectionStale
	}
	pinned, err := collectionDigest(expected)
	if err != nil {
		return err
	}
	current, err := collectionDigest(receipt.ResultCollection)
	if err != nil || current != pinned || expected.State != ResultCollectionPrepared {
		return errResultCollectionStale
	}
	if _, err := c.loadCollectionWork(ctx, item, expected); err != nil {
		return err
	}
	canonical := cloneTaskResult(result)
	canonical.TaskID, canonical.Attempt, canonical.Agent = tx.identity.TaskID, tx.identity.Attempt, tx.identity.Agent
	if err := c.attachCollectedTranscript(ctx, item, expected, canonical); err != nil {
		return err
	}
	body, err := json.Marshal(canonical, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return err
	}
	next := cloneResultCollectionCheckpoint(expected)
	next.Revision++
	next.Accepted, err = c.putCollectionBlob(ctx, next, "accepted", body)
	if err != nil {
		return err
	}
	next.State = ResultCollectionCommitted
	if err := c.appendCollectionCheckpointLocked(ctx, item, receipt, next, canonical); err != nil {
		return err
	}
	tx.controller.mu.Lock()
	tx.controller.result = canonical
	tx.controller.reserved = false
	tx.controller.pending = nil
	tx.controller.mu.Unlock()
	return c.publishTaskResultProjectionLocked(tx.identity, canonical, tx.controller)
}
