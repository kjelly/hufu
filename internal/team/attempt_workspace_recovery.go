package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// attemptWorldLastStateUnrecorded is orphan_removed's last_state for a world
// whose prepared event was never written (a crash during the copy).
const attemptWorldLastStateUnrecorded = "unrecorded"

// attemptApplyStartedPayload is what attempt_workspace_apply_started
// persists: enough to complete the task without re-running its worker.
type attemptApplyStartedPayload struct {
	WorldID           string              `json:"world_id"`
	DeltaDigest       string              `json:"delta_digest"`
	TypedResult       *TaskResult         `json:"typed_result"`
	VerifyResult      *VerificationResult `json:"verify_result"`
	ExecutionReceipt  *ExecutionReceipt   `json:"execution_receipt"`
	CoordinatorOutput string              `json:"coordinator_output"`
}

// reconcileAttemptWorlds settles the attempt worlds a previous process left
// under the control root. Every public entry point that can dispatch a worker
// calls it after the task journal is initialized and before any dispatch or
// resume decision, whatever the execution profile: re-running a task whose
// changes were already applied would repeat that work on top of them.
//
// A world's state comes from the session's global events; a branch-filtered
// view would hide a world another branch left mid-apply. Only worlds of the
// current branch are acted on. A world that is applying, or applied while
// its task is not done, is never deleted. Nothing outside the attempt-worlds
// directory is touched.
func (c *Coordinator) reconcileAttemptWorlds(ctx context.Context) error {
	if c == nil || c.session == nil || !c.session.Scope.Managed || strings.TrimSpace(c.session.Scope.ControlRoot) == "" || !c.hasDurableEventJournal() {
		return nil
	}
	worldsDir := attemptWorldsDir(c.session.Scope.ControlRoot)
	entries, err := os.ReadDir(worldsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list attempt worlds: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return fmt.Errorf("read attempt world events: %w", err)
	}
	records := attemptWorldRecordsFromEvents(events)
	visible := c.currentBranchEventIDs(events)
	latestByTask := make(map[string]*attemptWorldRecord)
	for _, record := range records {
		if !visible[record.PreparedEventID] {
			continue
		}
		if latest := latestByTask[record.TaskID]; latest == nil || record.order > latest.order {
			latestByTask[record.TaskID] = record
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		worldDir := filepath.Join(worldsDir, entry.Name())
		if isLiveAttemptWorld(worldDir) {
			continue // an attempt in this process is using it
		}
		owner, err := readAttemptWorldOwner(worldDir)
		if err != nil {
			log.Printf("warning: %s is not a Hufu attempt world (%v); leaving it in place", worldDir, err)
			continue
		}
		if !sameProjectRoot(owner.SourceRoot, c.projectDir) {
			log.Printf("warning: attempt world %s belongs to project %s, not %s; leaving it in place", worldDir, owner.SourceRoot, c.projectDir)
			continue
		}
		record := records[owner.WorldID]
		if record != nil && !visible[record.PreparedEventID] {
			c.warnForeignAttemptWorld(worldDir, owner, record)
			continue
		}
		if err := c.reconcileAttemptWorld(ctx, worldDir, owner, record, latestByTask, visible); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) reconcileAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, latestByTask map[string]*attemptWorldRecord, visible map[string]bool) error {
	if record == nil {
		// No prepared event: the process stopped while copying, before a
		// worker could run. A delta file means an apply may have begun, so
		// that world is kept for inspection instead.
		if _, err := os.Lstat(filepath.Join(worldDir, attemptWorldDeltaFile)); err == nil {
			log.Printf("warning: attempt world %s has no recorded lifecycle but holds a delta; leaving it in place", worldDir)
			return nil
		}
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, attemptWorldLastStateUnrecorded)
		return nil
	}
	item := c.todoItemByID(record.TaskID)
	switch record.State {
	case attemptWorldStateApplying, attemptWorldStateApplied:
		if record.State == attemptWorldStateApplied && item != nil && item.Status == TaskDone {
			c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
			return nil
		}
		startVisible := visible[record.ApplyStarted.ID]
		if record.State == attemptWorldStateApplied {
			startVisible = startVisible && visible[record.Applied.ID]
		}
		return c.reconcileCommittedAttemptWorld(ctx, worldDir, owner, record, item, startVisible)
	case attemptWorldStatePrepared:
		if item != nil && item.Status == TaskBlocked && item.ExecutionReceipt != nil && item.ExecutionReceipt.ExecutionWorldID == record.WorldID {
			return nil
		}
		if checkpoint := currentResultCollection(item); checkpoint != nil && item.Status != TaskDone && checkpoint.Attempt == record.Attempt && checkpoint.SourceRunID == record.RunID {
			if isCommittedNormalizedCollection(item) && CanTransition(item.Status, TaskDone) {
				return c.reconcilePreparedCollectedApply(ctx, worldDir, owner, record, item, checkpoint)
			}
			return nil
		}
		if item != nil && latestByTask[record.TaskID] == record && record.OccurrenceAttempt == item.Retries+1 {
			if item.Status == TaskProtocolIncomplete || c.returnedCollectionNeedsPreparedWorld(ctx, item, record) || c.normalizationNeedsPreparedWorld(item, record) {
				return nil // the result continuation can settle this world on resume
			}
		}
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
	default:
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
	}
	return nil
}

func (c *Coordinator) normalizationNeedsPreparedWorld(item *TodoItem, record *attemptWorldRecord) bool {
	if isNormalizedVerificationPending(item) || (item.Status == TaskInProgress && canResumeDirectNormalization(item)) {
		receipt := item.ExecutionReceipt
		return receipt == nil || (receipt.ExecutionWorldID == record.WorldID && receipt.RunID == record.RunID)
	}
	accepted, err := c.readAcceptedNormalizedSubmission(item)
	if err != nil {
		log.Printf("warning: cannot check task %s accepted submission (%v); keeping attempt world %s", item.ID, err, record.WorldID)
		return true
	}
	if accepted != nil {
		return accepted.ExecutionWorldID == record.WorldID && accepted.RunID == record.RunID
	}
	checkpoint, err := c.readDirectNormalizationCheckpoint(item)
	if err != nil {
		log.Printf("warning: cannot check task %s normalization checkpoint (%v); keeping attempt world %s", item.ID, err, record.WorldID)
		return true
	}
	return checkpoint != nil && checkpoint.Receipt.ExecutionWorldID == record.WorldID && checkpoint.Receipt.RunID == record.RunID
}

func (c *Coordinator) reconcileCommittedAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem, visible bool) error {
	if item == nil || !CanTransition(item.Status, TaskDone) {
		log.Printf("warning: attempt world %s is %s but task %s cannot be completed from this session; leaving it in place", worldDir, record.State, record.TaskID)
		return nil
	}
	normalized := item.ResultContract != nil && item.ResultContract.Normalizer != ""
	var started attemptApplyStartedPayload
	var delta *AttemptWorkspaceDelta
	err := json.Unmarshal(record.ApplyStarted.Payload, &started)
	if normalized {
		if err == nil {
			if collection := currentResultCollection(item); collection != nil {
				delta, err = c.validateCollectedApplyReplay(ctx, worldDir, owner, record, item, collection, started, visible)
			} else {
				delta, err = c.validateNormalizedApplyReplay(ctx, worldDir, owner, record, item, started, visible)
			}
		}
		if err != nil {
			return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectNormalizationRecoveryRead(err))
		}
	} else if err != nil {
		detail := c.FailureDetail(fmt.Errorf("%s: attempt world %s has no readable completion record: %w", workspaceApplyIncompleteCode, owner.WorldID, err), FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked, item.Output)
		return nil
	}
	if record.State == attemptWorldStateApplying {
		return c.recoverApplyingAttemptWorld(ctx, worldDir, owner, record, item, started, delta)
	}
	return c.finishRecoveredAttempt(ctx, worldDir, owner, item, started, normalized)
}

func (c *Coordinator) validateCollectedApplyReplay(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem, cp *ResultCollectionCheckpoint, started attemptApplyStartedPayload, visible bool) (*AttemptWorkspaceDelta, error) {
	if !visible || cp.SourceRunID != record.RunID || cp.TaskID != record.TaskID || cp.Attempt != record.Attempt || cp.BranchID != record.BranchID ||
		owner.WorldID != record.WorldID || owner.RunID != record.RunID || owner.TaskID != item.ID || owner.Attempt != record.Attempt ||
		owner.OccurrenceAttempt != record.OccurrenceAttempt || record.OccurrenceAttempt != item.Retries+1 ||
		record.ApplyStarted.Actor != attemptWorldEventActor || record.ApplyStarted.TaskID != cp.TaskID || record.ApplyStarted.Attempt != cp.Attempt {
		return nil, errors.New("isolated apply has no matching original collection work")
	}
	checkpoint, intent, err := c.readCollectedApply(cp, worldDir)
	if err != nil {
		return nil, err
	}
	if checkpoint == nil || !intent.published || !checkpoint.matches(started) {
		return nil, errors.New("collection apply event differs from its sealed verification")
	}
	if err := c.validateCollectedApplySources(ctx, item, cp, record.WorldID, started); err != nil {
		return nil, err
	}
	manifest, err := c.collectedVerifiedWorkspaceManifest(ctx, item, started, intent.published)
	if err != nil {
		return nil, err
	}
	if manifest != "" && manifest != checkpoint.ManifestSHA256 {
		return nil, errors.New("collection apply event does not match the originally verified manifest")
	}
	if err := verifyAppliedWorldEvent(record, cp.TaskID, started.DeltaDigest); err != nil {
		return nil, err
	}
	return checkpoint.readDelta(worldDir, record.WorldID, record.BaselineDigest)
}

func (c *Coordinator) validateCollectedApplySources(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, worldID string, started attemptApplyStartedPayload) error {
	receipt := started.ExecutionReceipt
	if cp.State != ResultCollectionCommitted || receipt == nil ||
		!sameCanonicalEvidence(cp, currentResultCollection(&TodoItem{ExecutionReceipt: receipt, Retries: item.Retries})) ||
		started.WorldID != worldID || receipt.ExecutionWorldID != worldID || receipt.RunID != cp.SourceRunID ||
		receipt.TaskID != cp.TaskID || receipt.ModelExecutionID != cp.ModelExecutionID || receipt.Attempt != cp.Attempt {
		return errors.New("isolated apply does not belong to the sealed collection world")
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	if err != nil {
		return err
	}
	if work.Receipt.ExecutionWorldID != worldID {
		return errors.New("isolated apply does not belong to the sealed collection world")
	}
	if err := c.validateCollectedApplyResult(ctx, item, cp, started); err != nil {
		return err
	}
	return c.validateNormalizedCollectionResultSources(ctx, cp, started.TypedResult, receipt)
}

func (c *Coordinator) validateNormalizedCollectionResultSources(ctx context.Context, cp *ResultCollectionCheckpoint, result *TaskResult, receipt *ExecutionReceipt) error {
	fanoutModelID := ""
	if cp.FanoutLeaf {
		fanoutModelID = cp.ModelExecutionID
	}
	projection, err := c.normalizedWorkMedia(ctx, collectionWorkIdentity(cp), fanoutModelID, result, receipt)
	if err != nil {
		return err
	}
	if !sameCanonicalEvidence(projection, result.RuntimeMedia) {
		return errors.New("isolated apply lost media from the original collection work")
	}
	return c.validateRequiredEvidenceDeliveryForResult(ctx, cp.TaskID, cp.Attempt, result, cp.SourceRunID)
}

func (c *Coordinator) validateCollectedApplyResult(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, started attemptApplyStartedPayload) error {
	accepted, err := c.readAcceptedCollectionResult(ctx, item, cp)
	if err != nil {
		return err
	}
	if started.TypedResult == nil {
		return errors.New("isolated apply lost its accepted collection result")
	}
	// The source check also authenticates this runtime-owned enrichment.
	if projection := started.TypedResult.RuntimeMedia; projection != nil {
		attachTaskRuntimeMedia(accepted, projection)
	}
	got, gotErr := marshalNormalizedCheckpoint(started.TypedResult)
	want, wantErr := marshalNormalizedCheckpoint(accepted)
	if gotErr != nil || wantErr != nil || string(got) != string(want) {
		return errors.New("isolated apply result differs from the sealed collection result")
	}
	return nil
}

func (c *Coordinator) validateNormalizedApplyReplay(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem, started attemptApplyStartedPayload, visible bool) (*AttemptWorkspaceDelta, error) {
	event := record.ApplyStarted
	receipt := item.ExecutionReceipt
	if receipt == nil || !visible || event.Actor != attemptWorldEventActor || event.TaskID != item.ID ||
		event.Attempt != record.Attempt || record.TaskID != item.ID || record.RunID != receipt.RunID ||
		owner.WorldID != started.WorldID || owner.WorldID != receipt.ExecutionWorldID || owner.RunID != record.RunID ||
		owner.TaskID != record.TaskID || owner.Attempt != record.Attempt || record.Attempt != receipt.Attempt ||
		owner.OccurrenceAttempt != record.OccurrenceAttempt || record.OccurrenceAttempt != item.Retries+1 {
		return nil, errors.New("normalized apply does not match its original attempt world")
	}
	result, err := c.normalizedVerificationResult(item)
	if err != nil {
		return nil, err
	}
	checkpoint, err := c.loadNormalizedCompletion(item.ID, result, "")
	if err != nil {
		return nil, err
	}
	if err := checkpoint.validateApplyEvidence(worldDir); err != nil {
		return nil, err
	}
	if !checkpoint.matchesApply(started) || !checkpoint.verified(item, started.VerifyResult) ||
		!sameCanonicalEvidence(receipt, started.ExecutionReceipt) || !sameCanonicalEvidence(result, started.TypedResult) {
		return nil, errors.New("normalized apply event does not match the committed completion evidence")
	}
	manifest, err := checkpoint.verifiedWorkspaceManifest(item, true)
	if err != nil {
		return nil, err
	}
	if manifest != "" && manifest != checkpoint.DeltaManifestSHA256 {
		return nil, errors.New("normalized apply event does not match the originally verified manifest")
	}
	if err := c.validateNormalizedCompletionSources(ctx, item); err != nil {
		return nil, err
	}
	if err := verifyAppliedWorldEvent(record, item.ID, checkpoint.DeltaSHA256); err != nil {
		return nil, err
	}
	return checkpoint.readDelta(worldDir, record.WorldID, record.BaselineDigest)
}

func verifyAppliedWorldEvent(record *attemptWorldRecord, taskID, digest string) error {
	if record.State != attemptWorldStateApplied {
		return nil
	}
	applied := record.Applied
	var payload attemptWorldEventPayload
	if applied.Actor != attemptWorldEventActor || applied.TaskID != taskID || applied.Attempt != record.Attempt ||
		json.Unmarshal(applied.Payload, &payload) != nil || payload.WorldID != record.WorldID || payload.DeltaDigest != digest {
		return errors.New("applied world event does not match the committed delta")
	}
	return nil
}

// recoverApplyingAttemptWorld re-runs an apply a crash interrupted. The apply
// is re-entrant: paths already at their final state are skipped. Anything it
// cannot prove blocks the task and keeps the world.
func (c *Coordinator) recoverApplyingAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem, started attemptApplyStartedPayload, delta *AttemptWorkspaceDelta) error {
	normalized := delta != nil
	world := NewIsolatedCopyExecutionWorld()
	prepared, _, err := world.Load(worldDir, record.BaselineDigest)
	if err == nil && delta == nil {
		delta, err = readAttemptWorldDelta(worldDir, record.DeltaDigest)
	}
	if err != nil && normalized {
		return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectNormalizationRecoveryRead(err))
	}
	var result AttemptWorkspaceApplyResult
	if err == nil {
		result, err = applyAttemptWorkspaceDelta(context.WithoutCancel(ctx), c.projectDir, prepared.Root, delta)
	}
	a := &isolatedAttempt{world: world, prepared: prepared, taskID: record.TaskID, attempt: record.Attempt, state: isolatedAttemptRetained}
	if err == nil {
		err = c.appendAttemptWorldEvent(context.WithoutCancel(ctx), EventAttemptWorkspaceApplied, a, map[string]any{
			"world_id": owner.WorldID, "delta_digest": delta.Digest,
			"files_written": result.FilesWritten, "files_deleted": result.FilesDeleted, "recovered": true,
		})
		if err != nil && normalized {
			return rejectLocalResultPersistence(fmt.Errorf("record recovered apply of attempt world %s for task %s: %w", owner.WorldID, item.ID, err))
		}
	}
	if err != nil {
		detail := c.FailureDetail(fmt.Errorf("%s: the interrupted apply of attempt world %s could not be completed (world kept at %s): %w", workspaceApplyIncompleteCode, owner.WorldID, worldDir, err), FailureSourceError)
		if normalized {
			switch {
			case errors.Is(err, errAttemptDeltaSourceChanged):
				return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectLocalResult(RepairFailureSource, err))
			default:
				if _, conflict := errors.AsType[*AttemptWorkspaceConflictError](err); !conflict {
					return rejectLocalResultPersistence(fmt.Errorf("resume verified workspace apply: %w", err))
				}
				blocked := TaskBlocked
				return c.persistFailureWithOutput(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureWorkspaceConflict, &blocked, item.Output)
			}
		}
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked, item.Output)
		c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(item.ID))
		return nil
	}
	return c.finishRecoveredAttempt(ctx, worldDir, owner, item, started, normalized)
}

// finishRecoveredAttempt completes the task of an applied world and then
// deletes the world. The world stays when the task cannot complete.
func (c *Coordinator) finishRecoveredAttempt(ctx context.Context, worldDir string, owner attemptWorldOwner, item *TodoItem, started attemptApplyStartedPayload, normalized bool) error {
	if err := c.finalizeAppliedAttempt(ctx, item, started); err != nil {
		if normalized {
			return rejectLocalResultPersistence(fmt.Errorf("complete task %s from applied attempt world %s: %w", item.ID, owner.WorldID, err))
		}
		log.Printf("warning: complete task %s from applied attempt world %s: %v", item.ID, owner.WorldID, err)
		return nil
	}
	if err := removeAttemptWorldDir(worldDir, owner.WorldID); err != nil {
		log.Printf("warning: remove attempt world %s: %v", owner.WorldID, err)
	}
	return nil
}

// finalizeAppliedAttempt completes a task whose verified attempt was applied
// before a crash, from the evidence apply_started recorded. It mirrors the
// tail of finishProtocolRepair. The worker is not re-run and verification is
// not repeated: both happened before the apply. The success path's other
// follow-ups (worker memory ingestion, STM, reflexion) are skipped.
func (c *Coordinator) finalizeAppliedAttempt(ctx context.Context, item *TodoItem, started attemptApplyStartedPayload) error {
	if started.TypedResult != nil {
		c.storeSubmittedTaskResult(item.ID, started.TypedResult)
	}
	if started.VerifyResult != nil {
		if err := c.taskTracker.TodoList().SetVerificationResult(item.ID, started.VerifyResult); err != nil {
			return fmt.Errorf("restore verification result: %w", err)
		}
	}
	if started.ExecutionReceipt != nil {
		if err := c.setAttemptReceipt(item.ID, started.ExecutionReceipt); err != nil {
			return fmt.Errorf("restore execution receipt: %w", err)
		}
	}
	output := started.CoordinatorOutput
	if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output, map[string]any{"recovered_from_attempt_world": started.WorldID}); err != nil {
		return fmt.Errorf("mark task done: %w", err)
	}
	c.recordTerminalTypedTaskResult(item.ID)
	c.reconcileTaskStatusProjection()
	if current := c.todoItemByID(item.ID); current != nil {
		c.reEvaluateAffectedCriteria(ctx, current)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("step").withAgent(item.Agent).withMessage("completed task from its applied isolated attempt after an interruption").withTodoID(item.ID))
	return nil
}

func (c *Coordinator) removeOrphanAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, lastState string) {
	if err := removeAttemptWorldDir(worldDir, owner.WorldID); err != nil {
		log.Printf("warning: remove orphan attempt world %s: %v", owner.WorldID, err)
		return
	}
	a := &isolatedAttempt{prepared: &PreparedExecutionWorld{ID: owner.WorldID}, taskID: owner.TaskID, attempt: owner.Attempt}
	if err := c.appendAttemptWorldEvent(context.WithoutCancel(ctx), EventAttemptWorkspaceOrphanRemoved, a, map[string]any{
		"world_id": owner.WorldID, "last_state": lastState,
	}); err != nil {
		log.Printf("warning: record removed orphan attempt world %s: %v", owner.WorldID, err)
	}
}

// warnForeignAttemptWorld reports a world another branch left behind. It is
// kept untouched: only resuming that branch's session can finish it.
func (c *Coordinator) warnForeignAttemptWorld(worldDir string, owner attemptWorldOwner, record *attemptWorldRecord) {
	message := fmt.Sprintf("attempt world %s (run %s, task %s, state %s) belongs to another session branch (%s); it was left in place — resume that session to finish it", worldDir, record.RunID, owner.TaskID, record.State, record.BranchID)
	log.Printf("warning: %s", message)
	c.report(c.newEvent("step").withMessage("warning: " + message))
}

// currentBranchEventIDs is the set of event IDs visible on the active
// session branch.
func (c *Coordinator) currentBranchEventIDs(events []RunEvent) map[string]bool {
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		tree = nil
	}
	visible := make(map[string]bool)
	for _, event := range FilterEventsForBranch(events, tree, c.activeBranchID()) {
		visible[event.ID] = true
	}
	return visible
}

func sameProjectRoot(recorded, project string) bool {
	resolve := func(path string) string {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	return recorded != "" && project != "" && resolve(recorded) == resolve(project)
}
