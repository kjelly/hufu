package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceSessionOps performs workspace-aware session operations (fork,
// checkout, restore, adopt, manual snapshot) for one control workspace
// (docs/archive/implementation-plans/workspace-versioning.md §18–§20).
//
// Callers must hold the team lock and the project lock, pass a writable
// event store, and run RecoverWorkspaceOperations first.
type WorkspaceSessionOps struct {
	Version   WorkspaceVersionContext
	Workspace string
	Tree      *SessionTree
	Events    *EventStore
	Store     *versionstore.Store
}

// WorkspaceForkResult reports a completed fork.
type WorkspaceForkResult struct {
	Branch       *SessionBranch
	Snapshot     versionstore.Snapshot
	Materialized bool
}

// WorkspaceCheckoutResult reports a completed checkout.
type WorkspaceCheckoutResult struct {
	Branch       *SessionBranch
	PreviousID   string
	Saved        *versionstore.Snapshot
	Head         versionstore.Snapshot
	Materialized versionstore.MaterializeResult
}

// workspaceVersionStageHook simulates a crash after a named stage in tests
// (§36 Phase 2 fault injection). It is nil in production.
var workspaceVersionStageHook func(stage string) error

func workspaceStage(stage string) error {
	if workspaceVersionStageHook != nil {
		return workspaceVersionStageHook(stage)
	}
	return nil
}

func (o *WorkspaceSessionOps) validate(ctx context.Context) (versionstore.SubjectState, error) {
	if o.Events == nil {
		return versionstore.SubjectState{}, fmt.Errorf("workspace session operation: event store is unavailable")
	}
	if o.Tree == nil || o.Store == nil || !o.Version.Active() {
		return versionstore.SubjectState{}, fmt.Errorf("%w: workspace session operation is not configured", versionstore.ErrVersionedWorkspaceUnresolved)
	}
	return o.Store.EnsureSubjectState(ctx, o.Version.SubjectRoot, o.Version.Mode)
}

func refuseWhileRecovering(state versionstore.SubjectState) error {
	if state.RecoveryRequired {
		return fmt.Errorf("%w: %s (%s); run `hufu workspace version restore <head>` or `hufu workspace version adopt`",
			versionstore.ErrWorkspaceRecoveryRequired, state.RecoveryCode, state.RecoveryDetail)
	}
	return nil
}

// commitAndPublish appends the durable commit event and then publishes the
// snapshot (I3: never the other way round).
func (o *WorkspaceSessionOps) commitAndPublish(ctx context.Context, snapshot versionstore.Snapshot, stats versionstore.CaptureStats, expectedGeneration int64) (versionstore.Snapshot, error) {
	event, err := appendWorkspaceSnapshotCommitted(ctx, o.Events, snapshot, stats)
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	if _, err = o.Store.PublishSnapshot(ctx, snapshot.ID, event.ID, expectedGeneration); err != nil {
		return versionstore.Snapshot{}, fmt.Errorf("publish workspace snapshot %s: %w", snapshot.ID, err)
	}
	return o.Store.GetSnapshot(ctx, snapshot.ID)
}

// saveLiveState makes branchID's head equal the live filesystem: a branch
// without a head gets a baseline, and drift from the head becomes a new
// snapshot. saved is the snapshot created by this call, if any.
func (o *WorkspaceSessionOps) saveLiveState(ctx context.Context, branchID string, reason versionstore.SnapshotReason) (head versionstore.Snapshot, saved *versionstore.Snapshot, err error) {
	current, meta, hasHead, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, branchID)
	if err != nil {
		return versionstore.Snapshot{}, nil, err
	}
	if !hasHead {
		req, reqErr := o.Version.captureRequest(ctx, branchID, "", versionstore.SnapshotBaseline)
		if reqErr != nil {
			return versionstore.Snapshot{}, nil, reqErr
		}
		baseline, stats, captureErr := o.Store.Capture(ctx, req)
		if captureErr != nil {
			return versionstore.Snapshot{}, nil, fmt.Errorf("capture baseline of %s: %w", branchID, captureErr)
		}
		if head, err = o.commitAndPublish(ctx, baseline, stats, 0); err != nil {
			return versionstore.Snapshot{}, nil, err
		}
		return head, &head, nil
	}
	req, err := o.Version.captureRequest(ctx, branchID, current.ID, reason)
	if err != nil {
		return versionstore.Snapshot{}, nil, err
	}
	drift, changed, stats, err := o.Store.CaptureIfChanged(ctx, req, current.RootTreeHash)
	if err != nil {
		return versionstore.Snapshot{}, nil, fmt.Errorf("capture live state of %s: %w", branchID, err)
	}
	if !changed {
		return current, nil, nil
	}
	if head, err = o.commitAndPublish(ctx, drift, stats, meta.Generation); err != nil {
		return versionstore.Snapshot{}, nil, err
	}
	return head, &head, nil
}

func (o *WorkspaceSessionOps) markMaterialized(ctx context.Context, snapshot versionstore.Snapshot) error {
	return o.Store.UpdateSubjectState(ctx, o.Version.SubjectRoot, func(state *versionstore.SubjectState) {
		state.MaterializedWorkspaceID = snapshot.WorkspaceID
		state.MaterializedBranchID = snapshot.BranchID
		state.MaterializedSnapshotID = snapshot.ID
	})
}

func (o *WorkspaceSessionOps) materialize(ctx context.Context, target, current versionstore.SnapshotID) (versionstore.MaterializeResult, error) {
	subtrees, err := o.Version.ExcludeSubtrees()
	if err != nil {
		return versionstore.MaterializeResult{}, err
	}
	return o.Store.Materialize(ctx, versionstore.MaterializeRequest{
		Root: o.Version.SubjectRoot, SnapshotID: target, CurrentSnapshotID: current, ExcludeSubtrees: subtrees,
	})
}

func (o *WorkspaceSessionOps) step(ctx context.Context, op versionstore.Operation, state versionstore.OperationState, to versionstore.SnapshotID) error {
	return o.Store.UpdateOperation(ctx, op.ID, versionstore.OperationUpdate{State: state, ToSnapshotID: to})
}

// resolveForkBase finds the snapshot a fork starts from (§18.1/§18.2, §23).
func (o *WorkspaceSessionOps) resolveForkBase(ctx context.Context, target string, activeHead versionstore.Snapshot) (versionstore.Snapshot, bool, error) {
	active := o.Tree.ActiveBranch
	if target == "" {
		return activeHead, false, nil
	}
	branchID, eventID := o.Tree.ResolveTarget(target, o.Events)
	if eventID == "" && o.Tree.GetBranch(branchID) == nil {
		return versionstore.Snapshot{}, false, fmt.Errorf("fork target %q not found (not a branch, label, or event ID)", target)
	}
	if branch := o.Tree.GetBranch(branchID); branch != nil {
		branchID = branch.ID
	}
	if eventID == "" && branchID == active {
		return activeHead, false, nil
	}
	var id versionstore.SnapshotID
	if eventID == "" {
		head, _, ok, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, branchID)
		if err != nil {
			return versionstore.Snapshot{}, false, err
		}
		if !ok {
			return versionstore.Snapshot{}, false, fmt.Errorf("%w: branch %q has no workspace snapshot (legacy branch; use --metadata-only)", versionstore.ErrWorkspaceSnapshotUnavailable, branchID)
		}
		id = head.ID
	} else {
		events, err := o.Events.ReadEvents()
		if err != nil {
			return versionstore.Snapshot{}, false, fmt.Errorf("read events: %w", err)
		}
		found := false
		if id, found, err = workspaceSnapshotAtEvent(events, o.Tree, branchID, eventID); err != nil {
			return versionstore.Snapshot{}, false, err
		}
		if !found {
			return versionstore.Snapshot{}, false, fmt.Errorf("%w: no workspace snapshot at or before event %s (use --metadata-only)", versionstore.ErrWorkspaceSnapshotUnavailable, eventID)
		}
	}
	base, err := o.Store.GetSnapshot(ctx, id)
	if err != nil {
		return versionstore.Snapshot{}, false, err
	}
	if base.State != versionstore.StatePublished {
		return versionstore.Snapshot{}, false, fmt.Errorf("%w: snapshot %s is %s", versionstore.ErrWorkspaceSnapshotUnavailable, id, base.State)
	}
	return base, true, nil
}

// Fork creates a branch whose workspace head reuses the base snapshot's tree
// (no bytes are copied) and activates it. A historical fork first saves the
// active branch's live state and then materializes the base.
func (o *WorkspaceSessionOps) Fork(ctx context.Context, name, target string) (WorkspaceForkResult, error) {
	state, err := o.validate(ctx)
	if err != nil {
		return WorkspaceForkResult{}, err
	}
	if err = refuseWhileRecovering(state); err != nil {
		return WorkspaceForkResult{}, err
	}
	active := o.Tree.ActiveBranch
	reason := versionstore.SnapshotExternalDrift
	if target != "" {
		reason = versionstore.SnapshotCheckoutSave
	}
	activeHead, _, err := o.saveLiveState(ctx, active, reason)
	if err != nil {
		return WorkspaceForkResult{}, err
	}
	base, historical, err := o.resolveForkBase(ctx, target, activeHead)
	if err != nil {
		return WorkspaceForkResult{}, err
	}
	SnapshotBranchState(o.Workspace, o.Tree, active)
	branch, err := o.Tree.CreateBranch(name, target, o.Events)
	if err != nil {
		return WorkspaceForkResult{}, fmt.Errorf("failed to fork branch: %w", err)
	}
	// FromSnapshotID records what the filesystem holds now (the active
	// head); the fork base is the child snapshot's parent.
	op, err := o.Store.CreateOperation(ctx, versionstore.Operation{
		WorkspaceID: o.Version.WorkspaceID, BranchID: branch.ParentID, Kind: versionstore.OperationFork,
		FromSnapshotID: activeHead.ID, TargetBranchID: branch.ID,
	})
	if err != nil {
		return WorkspaceForkResult{}, err
	}
	if err = SaveSessionTree(o.Workspace, o.Tree); err != nil {
		return WorkspaceForkResult{}, fmt.Errorf("save session tree: %w", err)
	}
	if err = workspaceStage("fork:child_saved"); err != nil {
		return WorkspaceForkResult{}, err
	}
	child, err := o.Store.AttachBranch(ctx, o.Version.WorkspaceID, branch.ID, base.ID, versionstore.SnapshotFork)
	if err != nil {
		return WorkspaceForkResult{}, err
	}
	if err = o.step(ctx, op, versionstore.OperationObjectsWritten, child.ID); err != nil {
		return WorkspaceForkResult{}, err
	}
	if child, err = o.commitAndPublish(ctx, child, versionstore.CaptureStats{}, 0); err != nil {
		return WorkspaceForkResult{}, err
	}
	if err = o.step(ctx, op, versionstore.OperationEventCommitted, ""); err != nil {
		return WorkspaceForkResult{}, err
	}
	if err = workspaceStage("fork:event_committed"); err != nil {
		return WorkspaceForkResult{}, err
	}
	result := WorkspaceForkResult{Branch: branch, Snapshot: child}
	if err = o.finishFork(ctx, op, branch, child, activeHead); err != nil {
		return result, err
	}
	result.Materialized = historical && child.RootTreeHash != activeHead.RootTreeHash
	return result, nil
}

// finishFork materializes the child's tree when it differs from what the
// filesystem holds (historical forks), rebuilds the session projection, and
// activates the child. Recovery re-enters here once the child's commit event
// is durable (§18.3).
func (o *WorkspaceSessionOps) finishFork(ctx context.Context, op versionstore.Operation, branch *SessionBranch, child, activeHead versionstore.Snapshot) error {
	if child.RootTreeHash != activeHead.RootTreeHash {
		if err := o.step(ctx, op, versionstore.OperationMaterializing, ""); err != nil {
			return err
		}
		if _, err := o.materialize(ctx, child.ID, activeHead.ID); err != nil {
			return fmt.Errorf("materialize fork base: %w", err)
		}
		if err := o.step(ctx, op, versionstore.OperationMaterialized, ""); err != nil {
			return err
		}
	}
	if err := MaterializeCompactionBranch(o.Workspace, branch.ParentID, branch.ID, branch.ForkEventID); err != nil {
		return fmt.Errorf("failed to materialize compaction state for branch %q: %w", branch.ID, err)
	}
	SnapshotBranchState(o.Workspace, o.Tree, branch.ID)
	if err := RebuildSessionForBranch(o.Workspace, o.Tree, o.Events, branch.ID); err != nil {
		return fmt.Errorf("failed to rebuild session for new branch: %w", err)
	}
	o.Tree.ActiveBranch = branch.ID
	if err := SaveSessionTree(o.Workspace, o.Tree); err != nil {
		return fmt.Errorf("failed to save session tree: %w", err)
	}
	if err := o.step(ctx, op, versionstore.OperationCompleted, ""); err != nil {
		return err
	}
	return o.markMaterialized(ctx, child)
}

// Checkout switches the session branch and the filesystem together (§19).
func (o *WorkspaceSessionOps) Checkout(ctx context.Context, target string) (WorkspaceCheckoutResult, error) {
	state, err := o.validate(ctx)
	if err != nil {
		return WorkspaceCheckoutResult{}, err
	}
	if err = refuseWhileRecovering(state); err != nil {
		return WorkspaceCheckoutResult{}, err
	}
	branch, err := o.Tree.ResolveCheckoutBranch(target, o.Events)
	if err != nil {
		return WorkspaceCheckoutResult{}, err
	}
	previous := o.Tree.ActiveBranch
	SnapshotBranchState(o.Workspace, o.Tree, previous)
	activeHead, saved, err := o.saveLiveState(ctx, previous, versionstore.SnapshotCheckoutSave)
	if err != nil {
		return WorkspaceCheckoutResult{}, err
	}
	result := WorkspaceCheckoutResult{Branch: branch, PreviousID: previous, Saved: saved, Head: activeHead}
	if branch.ID == previous {
		return result, o.finishCheckout(ctx, versionstore.Operation{}, branch, activeHead)
	}
	head, _, ok, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, branch.ID)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, fmt.Errorf("%w: branch %q has no workspace snapshot (legacy branch; use --metadata-only)", versionstore.ErrWorkspaceSnapshotUnavailable, branch.ID)
	}
	op, err := o.Store.CreateOperation(ctx, versionstore.Operation{
		WorkspaceID: o.Version.WorkspaceID, BranchID: previous, Kind: versionstore.OperationCheckout,
		FromSnapshotID: activeHead.ID, ToSnapshotID: head.ID, TargetBranchID: branch.ID,
	})
	if err != nil {
		return result, err
	}
	if err = o.step(ctx, op, versionstore.OperationMaterializing, ""); err != nil {
		return result, err
	}
	if result.Materialized, err = o.materialize(ctx, head.ID, activeHead.ID); err != nil {
		return result, fmt.Errorf("materialize branch %q: %w", branch.ID, err)
	}
	if err = o.step(ctx, op, versionstore.OperationMaterialized, ""); err != nil {
		return result, err
	}
	if err = workspaceStage("checkout:materialized"); err != nil {
		return result, err
	}
	result.Head = head
	return result, o.finishCheckout(ctx, op, branch, head)
}

// finishCheckout is the forward-completion half of a checkout (§19.2).
func (o *WorkspaceSessionOps) finishCheckout(ctx context.Context, op versionstore.Operation, branch *SessionBranch, head versionstore.Snapshot) error {
	o.Tree.ActiveBranch = branch.ID
	if err := RebuildSessionForBranch(o.Workspace, o.Tree, o.Events, branch.ID); err != nil {
		return fmt.Errorf("failed to rebuild session for branch %q: %w", branch.ID, err)
	}
	if err := SaveSessionTree(o.Workspace, o.Tree); err != nil {
		return fmt.Errorf("failed to save session tree: %w", err)
	}
	if op.ID != "" {
		if err := o.step(ctx, op, versionstore.OperationCompleted, ""); err != nil {
			return err
		}
	}
	return o.markMaterialized(ctx, head)
}

// Restore makes the active branch's workspace equal an earlier snapshot by
// appending a restore node (history is never rewritten, §20). Under
// recovery_required the live (unaccepted) state is captured only as a
// forensic pending snapshot that is orphaned afterwards; it drives which
// paths the restore removes but never becomes a head.
func (o *WorkspaceSessionOps) Restore(ctx context.Context, id versionstore.SnapshotID) (versionstore.Snapshot, versionstore.MaterializeResult, error) {
	state, err := o.validate(ctx)
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	target, err := o.Store.GetSnapshot(ctx, id)
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	if target.State != versionstore.StatePublished || target.WorkspaceID != o.Version.WorkspaceID {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, fmt.Errorf("%w: %s is %s in workspace %s", versionstore.ErrWorkspaceSnapshotUnavailable, id, target.State, target.WorkspaceID)
	}
	active := o.Tree.ActiveBranch
	head, current, forensic, err := o.restoreStartingPoint(ctx, active, state.RecoveryRequired)
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	op, err := o.Store.CreateOperation(ctx, versionstore.Operation{
		WorkspaceID: o.Version.WorkspaceID, BranchID: active, Kind: versionstore.OperationRestore,
		FromSnapshotID: current, TargetBranchID: active,
	})
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	node, err := o.Store.DeriveSnapshot(ctx, versionstore.DeriveRequest{
		WorkspaceID: o.Version.WorkspaceID, BranchID: active, Parent: head.ID, Source: target.ID, Reason: versionstore.SnapshotRestore,
	})
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	if err = o.step(ctx, op, versionstore.OperationObjectsWritten, node.ID); err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	_, meta, _, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, active)
	if err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	if node, err = o.commitAndPublish(ctx, node, versionstore.CaptureStats{}, meta.Generation); err != nil {
		return versionstore.Snapshot{}, versionstore.MaterializeResult{}, err
	}
	if err = o.step(ctx, op, versionstore.OperationMaterializing, ""); err != nil {
		return node, versionstore.MaterializeResult{}, err
	}
	if err = workspaceStage("restore:event_committed"); err != nil {
		return node, versionstore.MaterializeResult{}, err
	}
	materialized, err := o.materialize(ctx, node.ID, current)
	if err != nil {
		return node, materialized, fmt.Errorf("materialize restore: %w", err)
	}
	if forensic != "" {
		if err = o.Store.OrphanSnapshot(ctx, forensic); err != nil {
			return node, materialized, err
		}
	}
	if err = o.step(ctx, op, versionstore.OperationCompleted, ""); err != nil {
		return node, materialized, err
	}
	if err = o.clearRecovery(ctx); err != nil {
		return node, materialized, err
	}
	return node, materialized, o.markMaterialized(ctx, node)
}

// restoreStartingPoint returns the branch head the restore node descends
// from and the snapshot describing the live managed paths.
func (o *WorkspaceSessionOps) restoreStartingPoint(ctx context.Context, branchID string, recovering bool) (head versionstore.Snapshot, current, forensic versionstore.SnapshotID, err error) {
	if !recovering {
		head, _, err = o.saveLiveState(ctx, branchID, versionstore.SnapshotCheckoutSave)
		return head, head.ID, "", err
	}
	head, _, ok, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, branchID)
	if err != nil {
		return versionstore.Snapshot{}, "", "", err
	}
	if !ok {
		return versionstore.Snapshot{}, "", "", fmt.Errorf("%w: branch %q has no accepted head to restore from", versionstore.ErrWorkspaceSnapshotUnavailable, branchID)
	}
	req, err := o.Version.captureRequest(ctx, branchID, head.ID, versionstore.SnapshotCheckoutSave)
	if err != nil {
		return versionstore.Snapshot{}, "", "", err
	}
	live, _, err := o.Store.Capture(ctx, req)
	if err != nil {
		return versionstore.Snapshot{}, "", "", fmt.Errorf("capture unaccepted live state: %w", err)
	}
	return head, live.ID, live.ID, nil
}

// Adopt accepts the live state after a recovery-required stop as the active
// branch's new head (§22.3) and clears the marker.
func (o *WorkspaceSessionOps) Adopt(ctx context.Context) (versionstore.Snapshot, error) {
	state, err := o.validate(ctx)
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	if !state.RecoveryRequired {
		return versionstore.Snapshot{}, errors.New("workspace versioning is not waiting for recovery; nothing to adopt")
	}
	active := o.Tree.ActiveBranch
	head, meta, hasHead, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, active)
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	req, err := o.Version.captureRequest(ctx, active, head.ID, versionstore.SnapshotAdopt)
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	snapshot, stats, err := o.Store.Capture(ctx, req)
	if err != nil {
		return versionstore.Snapshot{}, fmt.Errorf("capture adopted state: %w", err)
	}
	expected := int64(0)
	if hasHead {
		expected = meta.Generation
	}
	if snapshot, err = o.commitAndPublish(ctx, snapshot, stats, expected); err != nil {
		return versionstore.Snapshot{}, err
	}
	if err = o.clearRecovery(ctx); err != nil {
		return snapshot, err
	}
	return snapshot, o.markMaterialized(ctx, snapshot)
}

// SnapshotNow records the live state of the active branch (manual snapshot).
func (o *WorkspaceSessionOps) SnapshotNow(ctx context.Context) (versionstore.Snapshot, bool, error) {
	state, err := o.validate(ctx)
	if err != nil {
		return versionstore.Snapshot{}, false, err
	}
	if err = refuseWhileRecovering(state); err != nil {
		return versionstore.Snapshot{}, false, err
	}
	head, saved, err := o.saveLiveState(ctx, o.Tree.ActiveBranch, versionstore.SnapshotManual)
	if err != nil {
		return versionstore.Snapshot{}, false, err
	}
	return head, saved != nil, o.markMaterialized(ctx, head)
}

func (o *WorkspaceSessionOps) clearRecovery(ctx context.Context) error {
	return o.Store.UpdateSubjectState(ctx, o.Version.SubjectRoot, func(state *versionstore.SubjectState) {
		state.RecoveryRequired, state.RecoveryCode, state.RecoveryDetail = false, "", ""
	})
}
