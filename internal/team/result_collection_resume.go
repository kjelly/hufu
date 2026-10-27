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
	if checkpoint == nil || checkpoint.Attempt != item.ExecutionReceipt.Attempt {
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
	checkpoint := currentResultCollection(item)
	if checkpoint == nil {
		return "", errResultCollectionStale
	}
	if item.Status == TaskDone {
		return item.Output, nil
	}
	if checkpoint.FanoutLeaf && !c.isModelFanoutLeaf(item.ID) {
		def, _, err := c.AgentPool().ResolveAgentName(item.Agent)
		if err != nil {
			return "", err
		}
		return c.executeTaskWithExtraModels(ctx, item.Agent, def, task, item.ID)
	}
	defer func() {
		if returnErr != nil {
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
		return "", err
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
		return "", err
	}
	work, err := c.loadCollectionWork(ctx, item, checkpoint)
	if err != nil {
		return "", err
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
			return "", err
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
		return "", err
	}
	result := c.GetTaskResult(item.ID)
	if result == nil {
		return "", &resultCollectionFailure{Reason: RepairFailureCommit, Checkpoint: checkpoint, Cause: fmt.Errorf("accepted collection result was not published")}
	}
	if err := validateCompletedTaskResult(result); err != nil {
		return "", withFailureClassOverride(err, FailureExecution)
	}
	if err := c.validateRequiredEvidenceDelivery(item.ID, checkpoint.Attempt); err != nil {
		return "", err
	}
	if HasBlockingInvariantAssessment(task.InvariantVerification, result.InvariantVerification) {
		return "", fmt.Errorf("runtime-attested invariant gate rejected the collected result")
	}
	output = coordinatorTaskOutput(string(work.Report), result)
	if taskUsesVerbatimTranscript(task) {
		output = formatVerbatimTranscriptManifest(result.RawOutputRef)
	}
	if task.AdversarialVerify > 0 && c.AgentPool().Sidecar() != nil {
		if err := c.adversarialVerify(ctx, task, item.ID, output); err != nil {
			return "", err
		}
	}
	return c.finishProtocolRepair(ctx, c.todoItemByID(item.ID), task, def.Name, work.Receipt.Model, result, output)
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
