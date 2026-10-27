package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// beginResultCollection persists the returned worker attempt before any
// collector invocation. Neither a completion claim nor a new work attempt is
// created here. The receipt and transcript are supplied by the runtime runner.
func (c *Coordinator) beginResultCollection(ctx context.Context, taskID string, receipt ExecutionReceipt, report string) (*ResultCollectionCheckpoint, error) {
	if c == nil || c.session == nil || c.session.Workspace == "" || !c.hasDurableEventJournal() {
		return nil, fmt.Errorf("result collection requires durable workspace storage")
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if err != nil {
		return nil, err
	}
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	controller := c.occurrenceController(taskID)
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	item := c.todoItemByID(taskID)
	if item == nil || item.Status != TaskInProgress || receipt.TaskID != taskID || receipt.RunID != identity.RunID || receipt.Attempt != identity.Attempt ||
		!strings.EqualFold(receipt.ProducerID, identity.Agent) || receipt.ModelExecutionID == "" || !validArtifactID(receipt.TranscriptRef) ||
		receipt.RunInputSnapshotID != item.RunInputSnapshotID || receipt.RunInputSnapshotHash != item.RunInputSnapshotHash {
		return nil, errResultCollectionStale
	}
	snapshot := controller.snapshot()
	if !snapshot.opened || snapshot.reserved || snapshot.result != nil || !sameTaskResultOccurrence(snapshot.identity, identity) {
		return nil, errTaskResultStale
	}
	if prior := collectionForReceipt(item, &receipt); prior != nil {
		if prior.WorkOccurrenceRevision != identity.OccurrenceRevision || prior.WorkDispatchID != identity.DispatchID {
			return nil, errResultCollectionStale
		}
		if _, err := c.loadCollectionWork(ctx, item, prior); err != nil {
			return nil, err
		}
		return cloneResultCollectionCheckpoint(prior), nil
	}
	policy, err := c.collectionPolicy(receipt.ExecutionTarget)
	if err != nil {
		return nil, err
	}
	if c.isModelFanoutLeaf(taskID) {
		target, err := c.modelFanoutTarget(taskID)
		if err != nil || target != receipt.ExecutionTarget {
			return nil, errResultCollectionStale
		}
	} else if item.ExecutionTarget != receipt.ExecutionTarget && !c.routeCandidateAllowed(item.ExecutionRoute, receipt.ExecutionTarget, "") {
		return nil, errResultCollectionStale
	}
	if err := c.validateResumeTranscript(ctx, &receipt); err != nil {
		return nil, err
	}
	contractHash, err := decisionTaskInputDigest(item)
	if err != nil {
		return nil, err
	}
	evidenceHash, err := c.resultCandidateEvidenceHash(item)
	if err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	workspaceHash, err := c.collectionWorkspaceIdentity(&receipt, workspace)
	if err != nil {
		return nil, err
	}
	checkpoint := &ResultCollectionCheckpoint{
		Version: resultCollectionVersion, Revision: 1, State: ResultCollectionPending,
		BranchID: c.activeBranchID(), SourceRunID: receipt.RunID, TaskID: taskID, Agent: identity.Agent, Attempt: receipt.Attempt,
		ModelExecutionID: receipt.ModelExecutionID, FanoutLeaf: c.isModelFanoutLeaf(taskID), ExecutionTarget: receipt.ExecutionTarget,
		WorkOccurrenceRevision: identity.OccurrenceRevision, WorkDispatchID: identity.DispatchID,
		SubmissionRevision: identity.OccurrenceRevision, SubmissionDispatchID: identity.DispatchID,
		PolicySHA256: policy.ConfigurationHash, TaskContractSHA256: contractHash,
		InputsID: item.RunInputSnapshotID, InputsSHA256: item.RunInputSnapshotHash, EvidenceSHA256: evidenceHash,
		WorkspaceIdentitySHA256: workspaceHash, CollectorSchemaRevision: resultCollectionVersion, TurnLimit: resultCollectionDefaultTurns,
	}
	bindingHash, err := collectionBindingDigest(checkpoint)
	if err != nil {
		return nil, err
	}
	original := cloneExecutionReceipt(&receipt)
	original.ResultCollection = nil
	body, err := json.Marshal(resultCollectionWork{Version: resultCollectionVersion, BindingSHA256: bindingHash, Receipt: original, Workspace: workspace, Report: []byte(utils.RedactSecrets(report))}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	checkpoint.Work, err = c.putCollectionBytes(ctx, checkpoint, "work", body, resultCollectionWorkMaxBytes)
	if err != nil {
		return nil, err
	}
	if err := c.appendCollectionCheckpointLocked(ctx, item, &receipt, checkpoint, nil); err != nil {
		return nil, err
	}
	return cloneResultCollectionCheckpoint(checkpoint), nil
}

func collectionForReceipt(item *TodoItem, receipt *ExecutionReceipt) *ResultCollectionCheckpoint {
	if item == nil || receipt == nil {
		return nil
	}
	for _, current := range item.ExecutionReceipts {
		if current.RunID == receipt.RunID && current.Attempt == receipt.Attempt && current.ModelExecutionID == receipt.ModelExecutionID {
			return current.ResultCollection
		}
	}
	if current := item.ExecutionReceipt; current != nil && current.RunID == receipt.RunID && current.Attempt == receipt.Attempt && current.ModelExecutionID == receipt.ModelExecutionID {
		return current.ResultCollection
	}
	return nil
}

func (c *Coordinator) loadCollectionWork(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint) (*resultCollectionWork, error) {
	if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	if item == nil || checkpoint.BranchID != c.activeBranchID() || checkpoint.TaskID != item.ID || checkpoint.Agent != strings.ToLower(strings.TrimSpace(item.Agent)) ||
		checkpoint.InputsID != item.RunInputSnapshotID || checkpoint.InputsSHA256 != item.RunInputSnapshotHash {
		return nil, errResultCollectionStale
	}
	policy, err := c.collectionPolicy(checkpoint.ExecutionTarget)
	if err != nil || policy.ConfigurationHash != checkpoint.PolicySHA256 {
		return nil, fmt.Errorf("collection policy changed: %w", errors.Join(err, errResultCollectionStale))
	}
	contractHash, err := decisionTaskInputDigest(item)
	if err != nil || contractHash != checkpoint.TaskContractSHA256 {
		return nil, fmt.Errorf("collection task contract changed: %w", errors.Join(err, errResultCollectionStale))
	}
	evidenceHash, err := c.resultCandidateEvidenceHash(item)
	if err != nil || evidenceHash != checkpoint.EvidenceSHA256 {
		return nil, fmt.Errorf("collection evidence sources changed: %w", errors.Join(err, errResultCollectionStale))
	}
	body, err := c.readCollectionBytes(ctx, checkpoint.Work, resultCollectionWorkMaxBytes)
	if err != nil {
		return nil, err
	}
	var work resultCollectionWork
	if err := json.Unmarshal(body, &work, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	bindingHash, err := collectionBindingDigest(checkpoint)
	if err != nil || work.Version != resultCollectionVersion || work.BindingSHA256 != bindingHash || work.Receipt.ResultCollection != nil ||
		work.Receipt.RunID != checkpoint.SourceRunID || work.Receipt.TaskID != item.ID || work.Receipt.Attempt != checkpoint.Attempt ||
		work.Receipt.ModelExecutionID != checkpoint.ModelExecutionID || work.Receipt.ExecutionTarget != checkpoint.ExecutionTarget ||
		!strings.EqualFold(work.Receipt.ProducerID, checkpoint.Agent) {
		return nil, errResultCollectionStale
	}
	root, err := filepath.Abs(c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, work.Workspace)
	if err != nil || !filepath.IsAbs(work.Workspace) || !filepath.IsLocal(relative) || (!checkpoint.FanoutLeaf && work.Workspace != root) {
		return nil, fmt.Errorf("collection worker workspace is outside its control root: %w", errResultCollectionStale)
	}
	workspaceHash, err := c.collectionWorkspaceIdentity(&work.Receipt, work.Workspace)
	if err != nil || workspaceHash != checkpoint.WorkspaceIdentitySHA256 {
		return nil, fmt.Errorf("collection workspace identity changed: %w", errors.Join(err, errResultCollectionStale))
	}
	if err := c.validateResumeTranscript(ctx, &work.Receipt); err != nil {
		return nil, err
	}
	return &work, nil
}
