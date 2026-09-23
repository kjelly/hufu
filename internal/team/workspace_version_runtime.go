package team

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// Runtime checkpoints (docs/archive/implementation-plans/workspace-versioning.md
// §22, D1): v1 only checkpoints at quiescent run boundaries — admission of an
// invocation and just before run_finished — never per attempt, because
// native workers write the subject root concurrently.

// workspaceRunVersioning is the coordinator's binding for one invocation.
type workspaceRunVersioning struct {
	version   WorkspaceVersionContext
	workspace string
	events    *EventStore
	branchID  string
}

func (c *Coordinator) workspaceRun() (workspaceRunVersioning, bool) {
	if c == nil || c.session == nil || !c.session.WorkspaceVersion.Active() {
		return workspaceRunVersioning{}, false
	}
	run := workspaceRunVersioning{version: c.session.WorkspaceVersion, workspace: c.session.Workspace, events: c.eventStore, branchID: "main"}
	if run.events != nil {
		if branch := run.events.BranchID(); branch != "" {
			run.branchID = branch
		}
	}
	return run, true
}

// withOps opens the store and runs fn. Required mode relies on the project
// lock the coordinator holds for its lifetime; observe mode takes it only
// for the duration of fn and reports skipped when it is busy.
func (r workspaceRunVersioning) withOps(ctx context.Context, operation string, fn func(*WorkspaceSessionOps) error) (skipped bool, err error) {
	if r.events == nil {
		return false, fmt.Errorf("workspace versioning %s: the event store is unavailable", operation)
	}
	if r.version.Required() && !r.version.HoldsProjectLock {
		return false, fmt.Errorf("workspace versioning %s: required mode without the project lock", operation)
	}
	if !r.version.Required() {
		lock, lockErr := versionstore.TryLockProject(r.version.StateDir, versionstore.LockOwner{WorkspaceID: r.version.WorkspaceID, Operation: operation})
		if errors.Is(lockErr, versionstore.ErrVersionOperationInProgress) {
			return true, nil
		}
		if lockErr != nil {
			return false, lockErr
		}
		defer func() { _ = lock.Close() }()
	}
	store, err := r.version.OpenStore(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = store.Close() }()
	tree, err := LoadSessionTree(r.workspace)
	if err != nil {
		return false, fmt.Errorf("load session tree: %w", err)
	}
	return false, fn(&WorkspaceSessionOps{Version: r.version, Workspace: r.workspace, Tree: tree, Events: r.events, Store: store})
}

// admitWorkspaceVersion is the §22.1 admission of one invocation. In required
// mode any failure blocks the run; observe mode only logs.
func (c *Coordinator) admitWorkspaceVersion(ctx context.Context) error {
	run, ok := c.workspaceRun()
	if !ok {
		return nil
	}
	skipped, err := run.withOps(ctx, "admission", func(ops *WorkspaceSessionOps) error {
		return ops.admitRun(ctx, run.branchID)
	})
	switch {
	case err != nil && run.version.Required():
		return fmt.Errorf("workspace versioning admission: %w", err)
	case err != nil:
		log.Printf("warning: workspace versioning (observe) admission skipped: %v", err)
	case skipped:
		log.Printf("warning: workspace versioning (observe) admission skipped: project lock is busy")
	}
	return nil
}

// checkpointWorkspaceVersion is the §22.2 run checkpoint, taken after every
// worker stopped and before run_finished. A failure never blocks
// run_finished: it marks the checkpoint deferred, and the next admission
// records the change as external drift.
func (c *Coordinator) checkpointWorkspaceVersion(ctx context.Context, result *RunResult) {
	run, ok := c.workspaceRun()
	if !ok {
		return
	}
	_, err := run.withOps(ctx, "run checkpoint", func(ops *WorkspaceSessionOps) error {
		return ops.checkpointRun(ctx, run.branchID)
	})
	if err == nil {
		return
	}
	c.deferWorkspaceCheckpoint(ctx, err)
	if result != nil {
		c.appendRunWarning(result, "workspace run checkpoint deferred", err)
	}
}

// deferWorkspaceCheckpoint records that the files of this run were not
// checkpointed (a failed run checkpoint or an interrupted run).
func (c *Coordinator) deferWorkspaceCheckpoint(ctx context.Context, cause error) {
	run, ok := c.workspaceRun()
	if !ok {
		return
	}
	if err := updateSubjectMarker(ctx, run.version, func(state *versionstore.SubjectState) { state.CheckpointDeferred = true }); err != nil {
		log.Printf("warning: record deferred workspace checkpoint (%v): %v", cause, err)
	}
}

// markWorkspaceUnauthorizedMutation records the §22.3 recovery marker after
// an external provider changed paths outside its writable roots. Only paths
// are recorded, never content.
func (c *Coordinator) markWorkspaceUnauthorizedMutation(ctx context.Context, request AttemptRequest, delta WorkspaceDelta, cause error) {
	run, ok := c.workspaceRun()
	if !ok {
		return
	}
	detail := fmt.Sprintf("run %s task %s attempt %d: %s; paths: %s", request.RunID, request.TaskID, request.Attempt,
		cause, strings.Join(deltaPaths(delta, 10), ", "))
	err := updateSubjectMarker(ctx, run.version, func(state *versionstore.SubjectState) {
		state.RecoveryRequired, state.RecoveryCode, state.RecoveryDetail = true, "unauthorized_mutation", detail
	})
	if err != nil {
		log.Printf("warning: record workspace recovery marker: %v", err)
	}
}

func deltaPaths(delta WorkspaceDelta, limit int) []string {
	var paths []string
	for _, added := range delta.Added {
		paths = append(paths, added.Path)
	}
	for _, modified := range delta.Modified {
		paths = append(paths, modified.Path)
	}
	paths = append(paths, delta.Deleted...)
	if len(paths) > limit {
		paths = append(paths[:limit], fmt.Sprintf("(+%d more)", len(paths)-limit))
	}
	return paths
}

// updateSubjectMarker changes subject_state without the project lock: it is
// a single optimistic row update that never touches files or heads.
func updateSubjectMarker(ctx context.Context, version WorkspaceVersionContext, mutate func(*versionstore.SubjectState)) error {
	store, err := version.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if _, err = store.EnsureSubjectState(ctx, version.SubjectRoot, version.Mode); err != nil {
		return err
	}
	return store.UpdateSubjectState(ctx, version.SubjectRoot, mutate)
}

// admitRun settles pending snapshots, refuses to run over an unresolved
// recovery marker or incomplete operations, and makes the branch head equal
// the live files (baseline or external drift).
func (o *WorkspaceSessionOps) admitRun(ctx context.Context, branchID string) error {
	state, err := o.validate(ctx)
	if err != nil {
		return err
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	if _, err = o.Store.RecoverPendingSnapshots(ctx, o.Version.WorkspaceID, workspaceCommitLookup(events)); err != nil {
		return err
	}
	ops, err := o.Store.IncompleteOperations(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.WorkspaceID == o.Version.WorkspaceID {
			return fmt.Errorf("%w: %s operation %s is incomplete; run a hufu session command to recover it", versionstore.ErrWorkspaceRecoveryRequired, op.Kind, op.ID)
		}
	}
	if err = refuseWhileRecovering(state); err != nil {
		return err
	}
	head, _, err := o.saveLiveState(ctx, branchID, versionstore.SnapshotExternalDrift)
	if err != nil {
		return err
	}
	return o.Store.UpdateSubjectState(ctx, o.Version.SubjectRoot, func(next *versionstore.SubjectState) {
		next.CheckpointDeferred = false
		next.MaterializedWorkspaceID, next.MaterializedBranchID, next.MaterializedSnapshotID = head.WorkspaceID, head.BranchID, head.ID
	})
}

// checkpointRun records the files a finished run left behind. It is skipped
// while a recovery marker is set, so a rejected mutation never becomes a
// run_checkpoint.
func (o *WorkspaceSessionOps) checkpointRun(ctx context.Context, branchID string) error {
	state, err := o.validate(ctx)
	if err != nil {
		return err
	}
	if state.RecoveryRequired {
		return nil
	}
	head, _, err := o.saveLiveState(ctx, branchID, versionstore.SnapshotRunCheckpoint)
	if err != nil {
		return err
	}
	return o.markMaterialized(ctx, head)
}
