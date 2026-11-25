package team

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
)

// Seal the accepted canonical envelope and its tool acknowledgment under the
// original dispatch lease, before returning success to the worker.
type acceptedNormalizedSubmission struct {
	Branch   string                      `json:"branch"`
	Identity submitResultRuntimeIdentity `json:"identity"`
	Receipt  ExecutionReceipt            `json:"receipt"`
}

// Finalization may attach runtime-owned media and transcript evidence to the
// accepted submission. Seal exactly what is authorized for verification before
// publishing the lifecycle event that makes it visible on restart.
type normalizedVerificationPublication struct {
	Receipt ExecutionReceipt `json:"receipt"`
	Output  string           `json:"output"`
}

func (c *Coordinator) normalizedVerificationPublicationKey(source submitResultRuntimeIdentity) string {
	return "normalized-verification/" + directNormalizationCheckpointDocument(c.activeBranchID(), source.TaskID, source.OccurrenceRevision, source.DispatchID)
}

func (c *Coordinator) sealNormalizedVerificationPublication(ctx context.Context, item *TodoItem) error {
	source, ok := normalizationWorkEvidenceIdentity(item)
	if !ok || !normalizedWorkerVerificationHandoff(item.ResultContract, item.ExecutionReceipt, item.TypedResult) {
		return rejectResultSourceRead(errors.New("normalized verification publication has no accepted work identity"))
	}
	body, err := marshalNormalizedCheckpoint(normalizedVerificationPublication{Receipt: *item.ExecutionReceipt, Output: item.Output})
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	return c.sealNormalizedHandoff(ctx, c.normalizedVerificationPublicationKey(source), body)
}

func (c *Coordinator) readNormalizedVerificationPublication(item *TodoItem) (normalizedVerificationPublication, bool, error) {
	var publication normalizedVerificationPublication
	source, ok := normalizationWorkEvidenceIdentity(item)
	if !ok {
		return publication, false, errors.New("normalized verification publication has no original dispatch")
	}
	body, found, err := workspaceDocs(c.session.Workspace).getDocument(c.normalizedVerificationPublicationKey(source))
	if err != nil || !found {
		return publication, false, err
	}
	if err := json.Unmarshal(body, &publication, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)); err != nil {
		return publication, false, err
	}
	return publication, true, nil
}

func acceptedNormalizedSubmissionKey(branch string, identity submitResultRuntimeIdentity) string {
	return "accepted-normalized-submission/" + directNormalizationCheckpointDocument(branch, identity.TaskID, identity.OccurrenceRevision, identity.DispatchID)
}

func (c *Coordinator) checkpointAcceptedNormalizedSubmission(ctx context.Context, identity submitResultRuntimeIdentity, result *TaskResult) error {
	if !isAcceptedLocalResult(result) || !c.hasDurableEventJournal() {
		return nil
	}
	item := c.todoItemByID(identity.TaskID)
	if item == nil || item.Status != TaskInProgress || item.ResultContract == nil || item.ResultContract.Normalizer == "" || currentResultCollection(item) != nil {
		return nil
	}
	transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
	if transcript == nil {
		return rejectResultSourceRead(errors.New("normalized submission has no original transcript"))
	}
	callID, _ := ctx.Value(submitResultCallIDKey{}).(string)
	receipt, err := c.checkpointNormalizationWorkReceipt(ctx, item, identity, transcript, callID)
	if err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	receipt.HandoffState, receipt.SubmittedResult = ResultHandoffSubmitted, result
	receipt.bindResultContractPayload(result)
	branch := c.activeBranchID()
	key := acceptedNormalizedSubmissionKey(branch, identity)
	body, err := marshalNormalizedCheckpoint(acceptedNormalizedSubmission{Branch: branch, Identity: identity, Receipt: receipt})
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	return c.sealNormalizedHandoff(ctx, key, body)
}

func marshalNormalizedCheckpoint(value any) ([]byte, error) {
	body, err := json.Marshal(value, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return redactJSONPreservingRuntimeOutputs(body)
}

func (c *Coordinator) sealNormalizedHandoff(ctx context.Context, key string, body []byte) error {
	written, err := c.writeNormalizationStage(ctx, key, nil, body)
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	if !written {
		existing, _, err := workspaceDocs(c.session.Workspace).getDocument(key)
		if err != nil {
			return rejectNormalizationRecoveryRead(err)
		}
		if !bytes.Equal(existing, body) {
			return rejectResultSourceRead(errors.New("normalized submission diverges from the committed handoff"))
		}
	}
	return nil
}

func (c *Coordinator) readAcceptedNormalizedSubmission(item *TodoItem) (*ExecutionReceipt, error) {
	if item == nil || item.Status != TaskInProgress || item.ResultContract == nil || item.ResultContract.Normalizer == "" || item.DispatchID == "" {
		return nil, nil
	}
	branch := c.activeBranchID()
	key := acceptedNormalizedSubmissionKey(branch, submitResultRuntimeIdentity{
		TaskID: item.ID, OccurrenceRevision: item.OccurrenceRevision, DispatchID: item.DispatchID,
	})
	body, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
	if err != nil || !found {
		return nil, err
	}
	var accepted acceptedNormalizedSubmission
	if err := json.Unmarshal(body, &accepted, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	original := *item
	original.ExecutionReceipt = &accepted.Receipt
	source, ok := normalizationWorkEvidenceIdentity(&original)
	result := accepted.Receipt.SubmittedResult
	if !ok || accepted.Branch != branch || !sameTaskResultOccurrence(source, accepted.Identity) ||
		source.OccurrenceRevision != item.OccurrenceRevision || source.DispatchID != item.DispatchID ||
		accepted.Receipt.HandoffState != ResultHandoffSubmitted || !normalizedWorkerAcceptedHandoff(item.ResultContract, &accepted.Receipt, result) ||
		accepted.Receipt.ModelExecutionID == "" || accepted.Receipt.OccurrenceAttempt != item.Retries+1 {
		return nil, errors.New("normalized handoff does not match the active work dispatch")
	}
	return &accepted.Receipt, nil
}

func (c *Coordinator) restoreAcceptedNormalizedSubmission(ctx context.Context, item *TodoItem) (*TodoItem, error) {
	receipt, err := c.readAcceptedNormalizedSubmission(item)
	if err != nil {
		return item, rejectNormalizationRecoveryRead(err)
	}
	if receipt == nil {
		return item, nil
	}
	result := receipt.SubmittedResult
	itemWithReceipt := *item
	itemWithReceipt.ExecutionReceipt = receipt
	itemWithReceipt.TypedResult = result
	if publication, found, err := c.readNormalizedVerificationPublication(&itemWithReceipt); err != nil {
		return item, rejectNormalizationRecoveryRead(err)
	} else if found {
		result, receipt = publication.Receipt.SubmittedResult, &publication.Receipt
		itemWithReceipt.Status, itemWithReceipt.Output = TaskVerifying, publication.Output
		itemWithReceipt.ExecutionReceipt, itemWithReceipt.TypedResult = receipt, result
		if _, err := c.normalizedVerificationResult(&itemWithReceipt); err != nil {
			return item, rejectNormalizationRecoveryRead(err)
		}
		c.storeSubmittedTaskResult(item.ID, result)
		if err := c.checkpointWorkerCompletion(ctx, item.ID, receipt, result, publication.Output, true); err != nil {
			return item, rejectLocalResultPersistence(err)
		}
		return c.todoItemByID(item.ID), nil
	}
	if _, err := c.normalizedVerificationResult(&itemWithReceipt); err != nil {
		return item, rejectNormalizationRecoveryRead(err)
	}
	if err := validateCompletedTaskResult(result); err != nil {
		if sourceErr := c.validateNormalizedCompletionSources(ctx, &itemWithReceipt); sourceErr != nil {
			return item, sourceErr
		}
		if persistErr := c.settleIncompleteNormalizedHandoff(item.ID, receipt, result, err); persistErr != nil {
			return item, persistErr
		}
		return c.todoItemByID(item.ID), nil
	}
	c.storeSubmittedTaskResult(item.ID, result)
	if err := c.setAttemptReceipt(item.ID, receipt); err != nil {
		return item, rejectLocalResultPersistence(err)
	}
	if err := c.checkpointNormalizedContinuation(ctx, item.ID, result.Summary); err != nil {
		return item, rejectLocalResultPersistence(err)
	}
	return c.todoItemByID(item.ID), nil
}

func (c *Coordinator) settleIncompleteNormalizedHandoff(taskID string, receipt *ExecutionReceipt, result *TaskResult, incomplete error) error {
	item := c.todoItemByID(taskID)
	failed := cloneExecutionReceipt(receipt)
	failed.ExitCode = new(1)
	failed.HandoffState, failed.SubmittedResult = ResultHandoffSubmitted, result
	c.storeSubmittedTaskResult(taskID, result)
	if err := c.setAttemptReceipt(taskID, &failed); err != nil {
		return rejectLocalResultPersistence(err)
	}
	status := TaskError
	if workerReportedBlocked(incomplete) {
		status = TaskBlocked
	}
	detail := c.FailureDetail(incomplete, FailureSourceError)
	if err := c.persistFailureWithOutput(item.Agent, item.Desc, taskID, detail, RetryNone, FailureExecution, &status, result.Summary); err != nil {
		return rejectLocalResultPersistence(err)
	}
	return nil
}
