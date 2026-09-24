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
		c.reconcileAttemptWorld(ctx, worldDir, owner, record, record != nil && latestByTask[record.TaskID] == record)
	}
	return nil
}

func (c *Coordinator) reconcileAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, latestOfTask bool) {
	if record == nil {
		// No prepared event: the process stopped while copying, before a
		// worker could run. A delta file means an apply may have begun, so
		// that world is kept for inspection instead.
		if _, err := os.Lstat(filepath.Join(worldDir, attemptWorldDeltaFile)); err == nil {
			log.Printf("warning: attempt world %s has no recorded lifecycle but holds a delta; leaving it in place", worldDir)
			return
		}
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, attemptWorldLastStateUnrecorded)
		return
	}
	item := c.todoItemByID(record.TaskID)
	switch record.State {
	case attemptWorldStateApplying:
		c.recoverApplyingAttemptWorld(ctx, worldDir, owner, record, item)
	case attemptWorldStateApplied:
		if item != nil && item.Status == TaskDone {
			c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
			return
		}
		c.finishRecoveredAttempt(ctx, worldDir, owner, record, item)
	case attemptWorldStatePrepared:
		if item != nil && item.Status == TaskProtocolIncomplete && latestOfTask && record.OccurrenceAttempt == item.Retries+1 {
			return // its result-only repair applies it on resume
		}
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
	default:
		c.removeOrphanAttemptWorld(ctx, worldDir, owner, record.State)
	}
}

// recoverApplyingAttemptWorld re-runs an apply a crash interrupted. The apply
// is re-entrant: paths already at their final state are skipped. Anything it
// cannot prove blocks the task and keeps the world.
func (c *Coordinator) recoverApplyingAttemptWorld(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem) {
	if item == nil || !CanTransition(item.Status, TaskDone) {
		log.Printf("warning: attempt world %s was mid-apply for task %s, which this session cannot complete; leaving it in place", worldDir, record.TaskID)
		return
	}
	world := NewIsolatedCopyExecutionWorld()
	prepared, _, err := world.Load(worldDir, record.BaselineDigest)
	var delta *AttemptWorkspaceDelta
	if err == nil {
		delta, err = readAttemptWorldDelta(worldDir, record.DeltaDigest)
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
	}
	if err != nil {
		detail := c.FailureDetail(fmt.Errorf("%s: the interrupted apply of attempt world %s could not be completed (world kept at %s): %w", workspaceApplyIncompleteCode, owner.WorldID, worldDir, err), FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked, item.Output)
		c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(item.ID))
		return
	}
	record.State = attemptWorldStateApplied
	c.finishRecoveredAttempt(ctx, worldDir, owner, record, item)
}

// finishRecoveredAttempt completes the task of an applied world and then
// deletes the world. The world stays when the task cannot complete.
func (c *Coordinator) finishRecoveredAttempt(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem) {
	if item == nil || !CanTransition(item.Status, TaskDone) {
		log.Printf("warning: attempt world %s was applied but task %s cannot be completed from this session; leaving it in place", worldDir, record.TaskID)
		return
	}
	var started attemptApplyStartedPayload
	if err := json.Unmarshal(record.ApplyStarted, &started); err != nil {
		detail := c.FailureDetail(fmt.Errorf("%s: the applied attempt world %s has no readable completion record: %w", workspaceApplyIncompleteCode, owner.WorldID, err), FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked, item.Output)
		return
	}
	if err := c.finalizeAppliedAttempt(ctx, item, started); err != nil {
		log.Printf("warning: complete task %s from applied attempt world %s: %v", item.ID, owner.WorldID, err)
		return
	}
	if err := removeAttemptWorldDir(worldDir, owner.WorldID); err != nil {
		log.Printf("warning: remove attempt world %s: %v", owner.WorldID, err)
	}
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
		if err := c.taskTracker.TodoList().SetExecutionReceipt(item.ID, started.ExecutionReceipt); err != nil {
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
