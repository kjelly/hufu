package team

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func returnedCollectionCanResume(item *TodoItem, err error) bool {
	if item == nil || localResultRejectionReason(err) != RepairFailureCommit {
		return false
	}
	if cp := currentResultCollection(item); cp != nil {
		return cp.State != ResultCollectionBlocked
	}
	if hasCurrentFanoutCollection(item) {
		return true
	}
	failure, ok := errors.AsType[*resultCollectionFailure](err)
	if !ok || failure.Checkpoint == nil || failure.Checkpoint.TaskID != item.ID {
		return false
	}
	cp := failure.Checkpoint
	if cp.FanoutLeaf {
		return cp.FanoutIndex >= 0 && cp.FanoutIndex < len(item.ExecutionTopology) &&
			item.ExecutionTopology[cp.FanoutIndex] == cp.ExecutionTarget
	}
	return cp.WorkOccurrenceRevision == item.OccurrenceRevision
}

func (c *Coordinator) returnedCollectionNeedsPreparedWorld(ctx context.Context, item *TodoItem, record *attemptWorldRecord) bool {
	if item.Status != TaskInProgress || len(item.ExecutionTopology) > 1 {
		return false
	}
	cp, err := c.readReturnedCollection(item, 0)
	if err != nil {
		return true // Storage must be readable before discarding possible returned work.
	}
	if cp == nil || cp.SourceRunID != record.RunID || cp.Attempt != record.Attempt {
		return false
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	return err != nil || work.Receipt.ExecutionWorldID == record.WorldID
}

func returnedCollectionDocument(branch, task string, revision, slot int) string {
	return fmt.Sprintf("result-collection-work:%s:%s:%d:%d", branch, task, revision, slot)
}

func (c *Coordinator) sealReturnedCollection(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint) error {
	if err := validateResultCollectionCheckpoint(checkpoint); err != nil {
		return err
	}
	slot := 0
	if checkpoint.FanoutLeaf {
		item = c.modelFanout.parent.todoItemByID(item.ID)
		slot = checkpoint.FanoutIndex + 1
	}
	if item == nil || item.Status != TaskInProgress {
		return errResultCollectionStale
	}
	name := returnedCollectionDocument(checkpoint.BranchID, item.ID, item.OccurrenceRevision, slot)
	body, err := json.Marshal(checkpoint, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return err
	}
	return c.sealCollectionDocument(ctx, name, body)
}

// Save a discoverable handoff before publication to the event journal.
func (c *Coordinator) sealCollectionDocument(ctx context.Context, name string, body []byte) error {
	return withRuntimeStore(c.artifactStoreRootPath(), func(store *runtimestore.Store) error {
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			prior, found, err := runtimestore.GetDocument(ctx, tx, name)
			if err != nil {
				return err
			}
			if !found {
				return runtimestore.PutDocument(ctx, tx, name, body)
			}
			if !bytes.Equal(prior, body) {
				return errResultCollectionStale
			}
			return nil
		})
	})
}

func (c *Coordinator) readReturnedCollection(item *TodoItem, slot int) (*ResultCollectionCheckpoint, error) {
	if slot < 0 || slot > len(item.ExecutionTopology) {
		return nil, errResultCollectionStale
	}
	branch := c.activeBranchID()
	name := returnedCollectionDocument(branch, item.ID, item.OccurrenceRevision, slot)
	body, found, err := workspaceDocs(c.artifactStoreRootPath()).getDocument(name)
	if err != nil || !found {
		return nil, err
	}
	var cp ResultCollectionCheckpoint
	if err := json.Unmarshal(body, &cp, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return nil, err
	}
	if err := validateResultCollectionCheckpoint(&cp); err != nil {
		return nil, err
	}
	if cp.BranchID != branch || cp.TaskID != item.ID || cp.State != ResultCollectionPending || cp.Revision != 1 ||
		cp.LegacySubmission != nil || cp.ContractUpgrade != nil || cp.FanoutLeaf != (slot > 0) {
		return nil, errResultCollectionStale
	}
	if cp.FanoutLeaf {
		if cp.FanoutIndex != slot-1 || cp.ExecutionTarget != item.ExecutionTopology[slot-1] {
			return nil, errResultCollectionStale
		}
	} else if cp.WorkOccurrenceRevision != item.OccurrenceRevision || item.DispatchID != "" && cp.WorkDispatchID != item.DispatchID {
		return nil, errResultCollectionStale
	}
	return &cp, nil
}

func collectionHandoffMatchesStart(cp *ResultCollectionCheckpoint, item *TodoItem, events []RunEvent) bool {
	attempt := cp.Attempt
	if cp.FanoutLeaf {
		attempt = 0 // Only the parent has a canonical lifecycle event.
	}
	return collectionStartMatches(cp.SourceRunID, cp.BranchID, item, attempt, events)
}

func collectionStartMatches(runID, branch string, item *TodoItem, expectedAttempt int, events []RunEvent) bool {
	for _, event := range slices.Backward(events) {
		attempt := eventDispatchAttempt(event)
		if event.TaskID != item.ID || attempt == 0 {
			continue
		}
		var start struct {
			Revision int `json:"occurrence_revision"`
		}
		return event.RunID == runID && (event.BranchID == branch || event.BranchID == "" && branch == "main") &&
			json.Unmarshal(event.Payload, &start) == nil && start.Revision == item.OccurrenceRevision &&
			(expectedAttempt == 0 || expectedAttempt == attempt)
	}
	return false
}

func (c *Coordinator) restoreReturnedCollections(ctx context.Context, item *TodoItem) error {
	if item.Status != TaskInProgress || !c.hasDurableEventJournal() || c.isModelFanoutLeaf(item.ID) {
		return nil
	}
	if len(item.ExecutionTopology) < 2 {
		if currentResultCollection(item) != nil {
			return nil
		}
		return c.restoreReturnedCollection(ctx, item, 0)
	}
	for i := range min(len(item.ExecutionTopology), maxConcurrentModels) {
		item = c.todoItemByID(item.ID)
		if currentFanoutCollection(item, i) != nil {
			continue
		}
		if err := c.restoreReturnedCollection(ctx, item, i+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) resumeReturnedCollectionTask(ctx context.Context, task TaskDef, item *TodoItem) (string, bool, error) {
	if item == nil {
		return "", false, nil
	}
	if err := c.restoreReturnedCollections(ctx, item); err != nil {
		return "", true, c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
	}
	current := c.todoItemByID(item.ID)
	if currentResultCollection(current) == nil && !hasCurrentFanoutCollection(current) {
		if current != nil {
			if err := c.incompleteMixedFanout(ctx, current, task); err != nil {
				return "", true, c.settleNormalizedRecoveryRead(current, task, rejectNormalizationRecoveryRead(err))
			}
		}
		return "", false, nil
	}
	output, err := c.resumeCollectedTask(ctx, task, current)
	return output, true, err
}

func (c *Coordinator) restoreReturnedCollection(ctx context.Context, item *TodoItem, slot int) error {
	cp, err := c.readReturnedCollection(item, slot)
	if err != nil || cp == nil {
		return err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return err
	}
	if !collectionHandoffMatchesStart(cp, item, events) {
		return errResultCollectionStale
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	if err != nil {
		return err
	}
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	controller := c.occurrenceController(item.ID)
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	current := c.todoItemByID(item.ID)
	if current == nil || current.Status != item.Status || current.OccurrenceRevision != item.OccurrenceRevision || current.DispatchID != item.DispatchID {
		return errResultCollectionStale
	}
	if prior := collectionForReceipt(current, &work.Receipt); prior != nil {
		return nil
	}
	if err := c.appendCollectionCheckpointLocked(ctx, current, &work.Receipt, cp, nil); err != nil {
		return rejectLocalResultPersistence(err)
	}
	return nil
}
