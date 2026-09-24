package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/kjelly/hufu/internal/tools"
)

// attemptWorldEventActor is the actor recorded on attempt-world events.
const attemptWorldEventActor = "attempt-world"

type isolatedAttemptState int

const (
	// isolatedAttemptPrepared: the world holds the attempt's work; the
	// canonical project has not been touched.
	isolatedAttemptPrepared isolatedAttemptState = iota
	// isolatedAttemptApplied: the changes are in the canonical project and
	// the world is kept until TaskDone commits.
	isolatedAttemptApplied
	// isolatedAttemptRetained: the world is kept on disk for recovery or
	// inspection (an incomplete apply, or an apply whose TaskDone did not
	// commit).
	isolatedAttemptRetained
	isolatedAttemptClosed
)

// isolatedAttempt is one attempt's prepared world. It is owned by the
// goroutine running the attempt and is not safe for concurrent use.
type isolatedAttempt struct {
	world    *IsolatedCopyExecutionWorld
	prepared *PreparedExecutionWorld
	taskID   string
	attempt  int
	state    isolatedAttemptState
}

func (a *isolatedAttempt) worldID() string  { return a.prepared.ID }
func (a *isolatedAttempt) worldDir() string { return a.prepared.isolated.worldDir }

type isolatedAttemptKey struct{}

// withIsolatedAttempt binds an attempt's tools and verification to its world
// and records the attempt on ctx for the completion path.
func (c *Coordinator) withIsolatedAttempt(ctx context.Context, a *isolatedAttempt) context.Context {
	if a == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, tools.AgentExecutionRootKey, tools.AgentExecutionRoot{
		Root:             a.prepared.Root,
		DeniedWriteRoots: []string{c.projectDir, attemptWorldsDir(c.session.Scope.ControlRoot)},
	})
	return context.WithValue(ctx, isolatedAttemptKey{}, a)
}

func isolatedAttemptFromContext(ctx context.Context) *isolatedAttempt {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(isolatedAttemptKey{}).(*isolatedAttempt)
	return a
}

// prepareIsolatedAttempt creates a new world for one dispatch attempt of an
// isolated task. It returns nil for a task that runs in the shared project.
func (c *Coordinator) prepareIsolatedAttempt(ctx context.Context, task TaskDef, todoID string, attempt int) (*isolatedAttempt, error) {
	if !task.WorkerWorkspace.isolated() {
		return nil, nil
	}
	if err := c.requireIsolatedWorkspaceScope(); err != nil {
		return nil, err
	}
	world := NewIsolatedCopyExecutionWorld()
	occurrenceAttempt := c.taskAttempt(todoID)
	prepared, err := world.Prepare(ctx, ExecutionWorldSpec{
		RunID: coordinatorRuntimeRunID(c), TaskID: todoID, Attempt: attempt, OccurrenceAttempt: occurrenceAttempt,
		Root: c.projectDir, ControlWorkspace: c.session.Scope.ControlRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare isolated attempt world: %w", err)
	}
	a := &isolatedAttempt{world: world, prepared: prepared, taskID: todoID, attempt: attempt}
	state := prepared.isolated
	if err := c.appendAttemptWorldEvent(ctx, EventAttemptWorkspacePrepared, a, map[string]any{
		"world_id": a.worldID(), "task_id": todoID, "occurrence_attempt": occurrenceAttempt, "attempt": attempt,
		"baseline_digest": state.baseline.digest(), "file_count": state.fileCount, "bytes": state.byteCount,
	}); err != nil {
		if releaseErr := world.Release(ctx, prepared); releaseErr != nil {
			log.Printf("warning: release unrecorded attempt world %s: %v", a.worldID(), releaseErr)
		}
		return nil, fmt.Errorf("record isolated attempt world: %w", err)
	}
	return a, nil
}

// isolatedApplyEvidence is what the task completes with once the attempt's
// changes are applied. attempt_workspace_apply_started persists it so a
// crash after the apply can finish the task without re-running the worker.
type isolatedApplyEvidence struct {
	TypedResult       *TaskResult
	VerifyResult      *VerificationResult
	Receipt           *ExecutionReceipt
	CoordinatorOutput string
}

// attemptWorkspaceApplyIncompleteError is an apply that may have written part
// of its changes. The task is blocked for a human and the world is kept.
type attemptWorkspaceApplyIncompleteError struct{ err error }

func (e *attemptWorkspaceApplyIncompleteError) Error() string { return e.err.Error() }
func (e *attemptWorkspaceApplyIncompleteError) Unwrap() error { return e.err }
func (e *attemptWorkspaceApplyIncompleteError) FailureClassOverride() TaskFailureClass {
	return FailureExecution
}

func isAttemptWorkspaceApplyIncomplete(err error) bool {
	var incomplete *attemptWorkspaceApplyIncompleteError
	return errors.As(err, &incomplete)
}

// integrateIsolatedAttempt applies a verified attempt's changes to the
// canonical project (integrate: on-verified). It runs after verification and
// before TaskDone, on every path that commits TaskDone. A conflict writes
// nothing and fails the attempt with workspace_conflict; an apply that fails
// part-way is retried once and otherwise blocks the task.
func (c *Coordinator) integrateIsolatedAttempt(ctx context.Context, a *isolatedAttempt, evidence isolatedApplyEvidence) error {
	if a == nil {
		return nil
	}
	if a.state != isolatedAttemptPrepared {
		return fmt.Errorf("%s: attempt world %s is not ready to apply", workspaceApplyIncompleteCode, a.worldID())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	delta, err := a.world.Delta(ctx, a.prepared)
	if err != nil {
		return withFailureClassOverride(err, FailurePolicy)
	}
	if len(delta.Changes) == 0 {
		// Nothing to apply: the world is released at once, and the task
		// completes exactly as a shared attempt would.
		a.state = isolatedAttemptApplied
		c.releaseIsolatedAttempt(a)
		return nil
	}
	if err := writeAttemptWorldDelta(a.worldDir(), delta); err != nil {
		return fmt.Errorf("%s: %w", workspaceApplyIncompleteCode, err)
	}
	// From here on the apply must run to completion: a cancellation between
	// two writes would leave a half-applied project.
	applyCtx := context.WithoutCancel(ctx)
	var receipt *ExecutionReceipt
	if evidence.Receipt != nil {
		cloned := cloneExecutionReceipt(evidence.Receipt)
		receipt = &cloned
	}
	if err := c.appendAttemptWorldEvent(applyCtx, EventAttemptWorkspaceApplyStarted, a, map[string]any{
		"world_id": a.worldID(), "delta_digest": delta.Digest, "change_count": len(delta.Changes),
		"typed_result": evidence.TypedResult, "verify_result": evidence.VerifyResult,
		"execution_receipt": receipt, "coordinator_output": evidence.CoordinatorOutput,
	}); err != nil {
		return fmt.Errorf("record isolated attempt apply: %w", err)
	}
	result, err := applyAttemptWorkspaceDelta(applyCtx, c.projectDir, a.prepared.Root, delta)
	var conflict *AttemptWorkspaceConflictError
	if errors.As(err, &conflict) {
		if eventErr := c.appendAttemptWorldEvent(applyCtx, EventAttemptWorkspaceApplyConflicted, a, map[string]any{
			"world_id": a.worldID(), "conflicted_paths": conflict.Paths, "conflict_count": conflict.Total,
		}); eventErr != nil {
			// The journal still says the apply started; keep the world so
			// recovery sees a consistent state instead of a missing root.
			a.state = isolatedAttemptRetained
			return &attemptWorkspaceApplyIncompleteError{err: fmt.Errorf("%s: record apply conflict: %w", workspaceApplyIncompleteCode, eventErr)}
		}
		a.state = isolatedAttemptClosed
		c.removeAttemptWorld(a)
		return withFailureClassOverride(err, FailureWorkspaceConflict)
	}
	if err != nil {
		// Writes are re-entrant: a path already at its final state is
		// skipped, so a second pass converges or reports the same failure.
		log.Printf("warning: isolated attempt apply for task %s failed, retrying once: %v", a.taskID, err)
		result, err = applyAttemptWorkspaceDelta(applyCtx, c.projectDir, a.prepared.Root, delta)
		if err != nil {
			a.state = isolatedAttemptRetained
			return &attemptWorkspaceApplyIncompleteError{err: fmt.Errorf("%s: apply isolated attempt changes (world kept at %s): %w", workspaceApplyIncompleteCode, a.worldDir(), err)}
		}
	}
	if err := c.appendAttemptWorldEvent(applyCtx, EventAttemptWorkspaceApplied, a, map[string]any{
		"world_id": a.worldID(), "delta_digest": delta.Digest,
		"files_written": result.FilesWritten, "files_deleted": result.FilesDeleted,
	}); err != nil {
		a.state = isolatedAttemptRetained
		return &attemptWorkspaceApplyIncompleteError{err: fmt.Errorf("%s: record applied changes (world kept at %s): %w", workspaceApplyIncompleteCode, a.worldDir(), err)}
	}
	a.state = isolatedAttemptApplied
	return nil
}

// releaseIsolatedAttempt deletes an applied world once its TaskDone has
// committed. A crash before this leaves the world for recovery to finish.
func (c *Coordinator) releaseIsolatedAttempt(a *isolatedAttempt) {
	if a == nil || a.state != isolatedAttemptApplied {
		return
	}
	a.state = isolatedAttemptClosed
	c.removeAttemptWorld(a)
}

// closeIsolatedAttempt ends an attempt that will not complete the task. A
// world whose changes were never applied is discarded; an applied or
// retained world stays on disk, because recovery needs it.
func (c *Coordinator) closeIsolatedAttempt(ctx context.Context, a *isolatedAttempt, reason string) {
	if a == nil {
		return
	}
	switch a.state {
	case isolatedAttemptPrepared:
		a.state = isolatedAttemptClosed
		if err := c.appendAttemptWorldEvent(context.WithoutCancel(ctx), EventAttemptWorkspaceDiscarded, a, map[string]any{
			"world_id": a.worldID(), "reason": reason,
		}); err != nil {
			log.Printf("warning: record discarded attempt world %s: %v", a.worldID(), err)
		}
		c.removeAttemptWorld(a)
	case isolatedAttemptApplied:
		a.state = isolatedAttemptClosed
		log.Printf("warning: attempt world %s was applied but task %s did not complete; keeping it for recovery", a.worldID(), a.taskID)
	case isolatedAttemptRetained:
		a.state = isolatedAttemptClosed
		log.Printf("warning: keeping attempt world %s for task %s at %s", a.worldID(), a.taskID, a.worldDir())
	}
}

func (c *Coordinator) removeAttemptWorld(a *isolatedAttempt) {
	if err := a.world.Release(context.Background(), a.prepared); err != nil {
		log.Printf("warning: remove attempt world %s: %v", a.worldID(), err)
	}
}

func (c *Coordinator) appendAttemptWorldEvent(ctx context.Context, eventType EventType, a *isolatedAttempt, payload map[string]any) error {
	if !c.hasDurableEventJournal() {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	if _, err := c.EventJournal().Append(ctx, RunEvent{
		Type:           string(eventType),
		Actor:          attemptWorldEventActor,
		TaskID:         a.taskID,
		Attempt:        a.attempt,
		IdempotencyKey: fmt.Sprintf("%s:%s:%s", attemptWorldEventActor, a.worldID(), eventType),
		Payload:        raw,
	}); err != nil {
		return fmt.Errorf("append %s: %w", eventType, err)
	}
	return nil
}

// attemptWorldDirFor is the directory of worldID under the control root.
func (c *Coordinator) attemptWorldDirFor(worldID string) string {
	return filepath.Join(attemptWorldsDir(c.session.Scope.ControlRoot), worldID)
}
