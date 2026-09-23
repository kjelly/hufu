package team

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceRecoveryReport lists what RecoverWorkspaceOperations repaired.
type WorkspaceRecoveryReport struct {
	Snapshots  []versionstore.RecoveryAction
	Completed  []versionstore.OperationID
	Failed     []versionstore.OperationID
	Removed    []string
	Unresolved []versionstore.OperationID
}

// Empty reports whether recovery had nothing to do.
func (r WorkspaceRecoveryReport) Empty() bool {
	return len(r.Snapshots)+len(r.Completed)+len(r.Failed)+len(r.Removed)+len(r.Unresolved) == 0
}

// RecoverWorkspaceOperations is the startup recovery of §16.2, §18.3, and
// §19.2. It runs before any other workspace version operation (coordinator
// startup and every mutating session / workspace version command):
//
//   - pending snapshots are published when their commit event is durable
//     and orphaned otherwise (never by appending an event, I13);
//   - an interrupted checkout or restore is completed forward;
//   - an interrupted fork is completed forward once the child's commit event
//     is durable, and otherwise its eventless inactive child branch is
//     removed and the operation fails.
func RecoverWorkspaceOperations(ctx context.Context, o *WorkspaceSessionOps) (WorkspaceRecoveryReport, error) {
	var report WorkspaceRecoveryReport
	if _, err := o.validate(ctx); err != nil {
		return report, err
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return report, fmt.Errorf("read events for workspace recovery: %w", err)
	}
	if report.Snapshots, err = o.Store.RecoverPendingSnapshots(ctx, o.Version.WorkspaceID, workspaceCommitLookup(events)); err != nil {
		return report, err
	}
	ops, err := o.Store.IncompleteOperations(ctx)
	if err != nil {
		return report, err
	}
	for _, op := range ops {
		if op.WorkspaceID != o.Version.WorkspaceID {
			continue
		}
		if err = o.recoverOperation(ctx, op, events, &report); err != nil {
			return report, fmt.Errorf("recover %s operation %s: %w", op.Kind, op.ID, err)
		}
	}
	return report, nil
}

func (o *WorkspaceSessionOps) recoverOperation(ctx context.Context, op versionstore.Operation, events []RunEvent, report *WorkspaceRecoveryReport) error {
	switch op.Kind {
	case versionstore.OperationCheckout:
		return o.recoverCheckout(ctx, op, report)
	case versionstore.OperationFork:
		return o.recoverFork(ctx, op, events, report)
	case versionstore.OperationRestore:
		return o.recoverRestore(ctx, op, report)
	default:
		// capture, adopt, downgrade, and gc keep no intermediate state that
		// needs forward completion; their snapshots were settled above.
		return o.failOperation(ctx, op, "interrupted", report)
	}
}

func (o *WorkspaceSessionOps) failOperation(ctx context.Context, op versionstore.Operation, code string, report *WorkspaceRecoveryReport) error {
	if err := o.Store.UpdateOperation(ctx, op.ID, versionstore.OperationUpdate{State: versionstore.OperationFailed, DetailCode: code}); err != nil {
		return err
	}
	report.Failed = append(report.Failed, op.ID)
	return nil
}

// recoverCheckout completes a checkout forward (§19.2): the filesystem may
// already be switched, so rolling back would be the guess the spec forbids.
func (o *WorkspaceSessionOps) recoverCheckout(ctx context.Context, op versionstore.Operation, report *WorkspaceRecoveryReport) error {
	branch := o.Tree.GetBranch(op.TargetBranchID)
	if branch == nil || op.ToSnapshotID == "" {
		return o.failOperation(ctx, op, "target_missing", report)
	}
	head, err := o.Store.GetSnapshot(ctx, op.ToSnapshotID)
	if err != nil {
		return err
	}
	if op.State != versionstore.OperationMaterialized {
		if _, err = o.materialize(ctx, head.ID, op.FromSnapshotID); err != nil {
			return fmt.Errorf("complete materialization: %w", err)
		}
		if err = o.step(ctx, op, versionstore.OperationMaterialized, ""); err != nil {
			return err
		}
	}
	if err = o.finishCheckout(ctx, op, branch, head); err != nil {
		return err
	}
	report.Completed = append(report.Completed, op.ID)
	return nil
}

// recoverRestore completes a restore whose restore node is canonical.
func (o *WorkspaceSessionOps) recoverRestore(ctx context.Context, op versionstore.Operation, report *WorkspaceRecoveryReport) error {
	if op.ToSnapshotID == "" {
		return o.failOperation(ctx, op, "no_restore_node", report)
	}
	node, err := o.Store.GetSnapshot(ctx, op.ToSnapshotID)
	if err != nil {
		return err
	}
	if node.State != versionstore.StatePublished {
		return o.failOperation(ctx, op, "restore_not_committed", report)
	}
	if _, err = o.materialize(ctx, node.ID, op.FromSnapshotID); err != nil {
		return fmt.Errorf("complete restore: %w", err)
	}
	if err = o.step(ctx, op, versionstore.OperationCompleted, ""); err != nil {
		return err
	}
	if err = o.clearRecovery(ctx); err != nil {
		return err
	}
	report.Completed = append(report.Completed, op.ID)
	return o.markMaterialized(ctx, node)
}

// recoverFork implements the §18.3 table.
func (o *WorkspaceSessionOps) recoverFork(ctx context.Context, op versionstore.Operation, events []RunEvent, report *WorkspaceRecoveryReport) error {
	branch := o.Tree.Branches[op.TargetBranchID]
	if branch == nil {
		return o.failOperation(ctx, op, "branch_not_saved", report)
	}
	var child versionstore.Snapshot
	if op.ToSnapshotID != "" {
		snapshot, err := o.Store.GetSnapshot(ctx, op.ToSnapshotID)
		if err != nil {
			return err
		}
		child = snapshot
	}
	if child.State == versionstore.StatePublished {
		current, err := o.Store.GetSnapshot(ctx, op.FromSnapshotID)
		if err != nil {
			return err
		}
		if err = o.finishFork(ctx, op, branch, child, current); err != nil {
			return err
		}
		report.Completed = append(report.Completed, op.ID)
		return nil
	}
	// The child's commit event never became durable. A child that is still
	// inactive and owns no events has no lineage of its own: removing it is
	// deterministic. Anything else stays and is reported.
	if o.Tree.ActiveBranch == branch.ID || branchOwnsEvents(events, branch.ID) {
		report.Unresolved = append(report.Unresolved, op.ID)
		return o.failOperation(ctx, op, "child_branch_kept", report)
	}
	delete(o.Tree.Branches, branch.ID)
	if err := SaveSessionTree(o.Workspace, o.Tree); err != nil {
		return fmt.Errorf("remove incomplete fork branch: %w", err)
	}
	report.Removed = append(report.Removed, branch.ID)
	return o.failOperation(ctx, op, "fork_not_committed", report)
}

func branchOwnsEvents(events []RunEvent, branchID string) bool {
	for _, event := range events {
		if event.BranchID == branchID {
			return true
		}
	}
	return false
}
