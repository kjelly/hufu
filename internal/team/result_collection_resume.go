package team

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// Only the current work attempt can prevent a fresh dispatch. Historical
// checkpoints remain audit evidence after a genuine execution retry.
func currentResultCollection(item *TodoItem) *ResultCollectionCheckpoint {
	if item == nil || item.ExecutionReceipt == nil {
		return nil
	}
	checkpoint := item.ExecutionReceipt.ResultCollection
	// Missing legacy occurrence provenance must not authorize repeating work.
	// Keep its existing recovery path; modern receipts distinguish lifecycle
	// retries from the work counter that restarts at each dispatch.
	occurrence := item.ExecutionReceipt.OccurrenceAttempt
	if checkpoint == nil || checkpoint.Attempt != item.ExecutionReceipt.Attempt || (occurrence > 0 && occurrence != item.Retries+1) {
		return nil
	}
	return cloneResultCollectionCheckpoint(checkpoint)
}

func (c *Coordinator) attachCollectedTranscript(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint, result *TaskResult) error {
	if !taskUsesVerbatimTranscript(taskDefFromTodoItem(item)) {
		return nil
	}
	work, err := c.loadCollectionWork(ctx, item, checkpoint)
	if err != nil {
		return err
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return err
	}
	ref, err := store.Get(ctx, work.Receipt.TranscriptRef)
	if err != nil {
		return err
	}
	transcript := &taskTranscript{todoID: checkpoint.TaskID, runID: checkpoint.SourceRunID, attempt: checkpoint.Attempt}
	return attachVerbatimTaskResult(&ref, transcript, result)
}

// resumeCollectedTask never dispatches worker work or enters legacy tool
// repair. It consumes the exact sealed work and the original collector budget.
func (c *Coordinator) resumeCollectedTask(ctx context.Context, task TaskDef, item *TodoItem) (output string, returnErr error) {
	if item.Status == TaskDone {
		return item.Output, nil
	}
	if !c.isModelFanoutLeaf(item.ID) && hasCurrentFanoutCollection(item) {
		def, _, err := c.AgentPool().ResolveAgentName(item.Agent)
		if err != nil {
			return "", err
		}
		return c.executeTaskWithExtraModels(ctx, item.Agent, def, task, item.ID)
	}
	checkpoint := currentResultCollection(item)
	if checkpoint == nil {
		return "", errResultCollectionStale
	}
	defer func() {
		if returnErr != nil {
			if ctx.Err() == context.Canceled && errors.Is(returnErr, context.Canceled) {
				detail := c.FailureDetail(returnErr, FailureSourceContextCanceled)
				returnErr = errors.Join(returnErr, c.PersistFailureWithClassAndStatusError(item.Agent, task.Goal, item.ID, detail, RetryNone, FailureCancelled, TaskError))
				return
			}
			if localResultRejectionReason(returnErr) == RepairFailureCommit {
				return
			}
			c.recordCollectionStopReceipt(item.ID, returnErr)
			current := c.todoItemByID(item.ID)
			if current != nil && (current.Status == TaskBlocked || current.Status == TaskError) {
				return
			}
			class := FailureProtocol
			if failure, ok := errors.AsType[interface {
				error
				FailureClassOverride() TaskFailureClass
			}](returnErr); ok {
				class = failure.FailureClassOverride()
			}
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID, c.FailureDetail(returnErr, FailureSourceError), NeedsHuman, class, TaskBlocked, item.Output)
		}
	}()

	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		return "", c.classifyCommittedCollectionRead(item, task, err)
	}
	admissionTask := task
	if checkpoint.FanoutLeaf {
		target, err := c.modelFanoutTarget(item.ID)
		if err != nil || target != checkpoint.ExecutionTarget || c.modelFanout.index != checkpoint.FanoutIndex {
			return "", errResultCollectionStale
		}
		// A leaf's model overlay is runtime-owned; admission still validates
		// the durable parent contract, while the indexed binding above proves
		// which exact admitted target owns these work bytes.
		admissionTask = taskDefFromTodoItem(item)
	}
	if _, _, err := c.validateTaskOccurrenceAdmission(ctx, admissionTask, item.ID, checkpoint.Attempt); err != nil {
		return "", c.classifyCommittedCollectionRead(item, task, err)
	}
	work, err := c.loadCollectionWork(ctx, item, checkpoint)
	if err != nil {
		return "", c.classifyCommittedCollectionRead(item, task, err)
	}
	if item.Status == TaskBlocked && !collectionGrantedForRun(collectionReceipt(item, checkpoint), c.executionRunID) {
		return "", &resultCollectionFailure{Reason: checkpoint.FailureReason, Checkpoint: checkpoint, Cause: errResultCollectionBlocked}
	}
	def, _, err := c.AgentPool().ResolveAgentName(item.Agent)
	if err != nil {
		return "", err
	}
	timeout := work.TimeoutSeconds
	if !work.GenerationCaptured {
		timeout = c.session.Config.Timeout
		if def.Timeout > 0 {
			timeout = def.Timeout
		}
	}
	if timeout <= 0 {
		timeout = 600
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, sessionResumeKey{}, true)
	var isolated *isolatedAttempt
	if item.WorkerWorkspace.isolated() {
		isolated, err = c.reopenCollectedWorld(ctx, item, checkpoint, work)
		if err != nil {
			return "", c.classifyCommittedCollectionRead(item, task, err)
		}
		ctx = c.withIsolatedAttempt(ctx, isolated)
		defer func() {
			if returnErr != nil {
				isolated.state = isolatedAttemptRetained
			}
			c.closeIsolatedAttempt(ctx, isolated, "collection_resume_exit")
		}()
	}
	if err := c.collectWorkResult(ctx, def, checkpoint); err != nil {
		return "", c.classifyCommittedCollectionRead(item, task, err)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil {
		return "", rejectLocalResultPersistence(errors.New("accepted collection result was not published"))
	}
	if err := validateCompletedTaskResult(result); err != nil {
		return "", withFailureClassOverride(err, FailureExecution)
	}
	if HasBlockingInvariantAssessment(task.InvariantVerification, result.InvariantVerification) {
		return "", fmt.Errorf("runtime-attested invariant gate rejected the collected result")
	}
	output = coordinatorTaskOutput(string(work.Report), result)
	if taskUsesVerbatimTranscript(task) {
		output = formatVerbatimTranscriptManifest(result.RawOutputRef)
	}
	current := c.todoItemByID(item.ID)
	if err := c.validateCollectedCompletionEvidence(ctx, current, task, checkpoint.Attempt, output); err != nil {
		return "", err
	}
	return c.finishProtocolRepair(ctx, current, task, def.Name, work.Receipt.Model, result, output)
}

func (c *Coordinator) validateCollectedCompletionEvidence(ctx context.Context, item *TodoItem, task TaskDef, attempt int, output string) error {
	if isCommittedNormalizedCollection(item) {
		return nil
	}
	if err := c.validateRequiredEvidenceDelivery(ctx, item.ID, attempt); err != nil {
		return err
	}
	if !c.isCommittedFanoutCollection(item) && task.AdversarialVerify > 0 && c.AgentPool().Sidecar() != nil {
		return c.adversarialVerify(ctx, task, item.ID, output)
	}
	return nil
}

func (c *Coordinator) isCommittedFanoutCollection(item *TodoItem) bool {
	cp := currentResultCollection(item)
	return cp != nil && cp.State == ResultCollectionCommitted && cp.FanoutLeaf && c.isModelFanoutLeaf(item.ID)
}

func (c *Coordinator) classifyCommittedCollectionRead(item *TodoItem, task TaskDef, err error) error {
	if !isCommittedNormalizedCollection(item) {
		if _, ok := errors.AsType[*resultCollectionFailure](err); ok {
			return err
		}
		return rejectNormalizationRecoveryRead(err)
	}
	return c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
}

func collectionWorkIdentity(cp *ResultCollectionCheckpoint) submitResultRuntimeIdentity {
	return submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt, Agent: cp.Agent,
		OccurrenceRevision: cp.WorkOccurrenceRevision, DispatchID: cp.WorkDispatchID}
}

func (c *Coordinator) verifyNormalizedCollectionTerminal(ctx context.Context, item *TodoItem, task TaskDef, output string) error {
	if c.terminalSessionMgr == nil {
		return nil
	}
	work, err := c.loadCollectionWork(ctx, item, currentResultCollection(item))
	if err != nil {
		return c.classifyCommittedCollectionRead(item, task, err)
	}
	return c.verifyNormalizedWorkTerminal(ctx, item, task, &work.Receipt, output)
}

func (c *Coordinator) finalizeNormalizedCollectionResult(ctx context.Context, item *TodoItem, result *TaskResult) (*TaskResult, error) {
	cp := currentResultCollection(item)
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	projection, err := c.verifyCollectedWorkEvidence(ctx, cp, result, &receipt)
	if err != nil {
		return nil, err
	}
	if projection == nil || result.RuntimeMedia != nil && receipt.RuntimeMediaHash == projection.Hash {
		return result, nil
	}
	return c.publishNormalizedWorkMedia(item.ID, projection, &receipt)
}

func (c *Coordinator) verifyCollectedWorkEvidence(ctx context.Context, cp *ResultCollectionCheckpoint, result *TaskResult, receipt *ExecutionReceipt) (*RuntimeMediaProjection, error) {
	fanoutModelID := ""
	if cp.FanoutLeaf {
		fanoutModelID = cp.ModelExecutionID
	}
	projection, err := c.normalizedWorkMedia(ctx, collectionWorkIdentity(cp), fanoutModelID, result, receipt)
	if err != nil {
		return nil, err
	}
	if err := c.validateRequiredEvidenceDeliveryForResult(ctx, cp.TaskID, cp.Attempt, result, cp.SourceRunID); err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	return projection, nil
}

func (c *Coordinator) reopenCollectedWorld(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint, work *resultCollectionWork) (*isolatedAttempt, error) {
	if !c.hasDurableEventJournal() || work.Receipt.ExecutionWorldID == "" {
		return nil, errResultCollectionStale
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return nil, err
	}
	record := attemptWorldRecordsFromEvents(events)[work.Receipt.ExecutionWorldID]
	if record == nil || record.TaskID != item.ID || record.RunID != checkpoint.SourceRunID || record.BranchID != checkpoint.BranchID || record.Attempt != checkpoint.Attempt || record.State != attemptWorldStatePrepared {
		return nil, errResultCollectionStale
	}
	world := NewIsolatedCopyExecutionWorld()
	prepared, owner, err := world.Load(c.attemptWorldDirFor(record.WorldID), record.BaselineDigest)
	if err != nil {
		return nil, err
	}
	if owner.TaskID != item.ID || owner.WorldID != record.WorldID {
		return nil, errResultCollectionStale
	}
	a := &isolatedAttempt{world: world, prepared: prepared, taskID: item.ID, attempt: checkpoint.Attempt}
	liveAttemptWorlds.Store(filepath.Clean(a.worldDir()), struct{}{})
	return a, nil
}
