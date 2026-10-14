package team

import (
	"context"
	"fmt"
	"maps"
)

// ForkSessionBranch forks a branch named name from target (the active branch
// when empty) and activates it. The parent's live snapshot, the child's row,
// its compaction state, its rebuilt session, and the active branch commit in
// one transaction: a failure leaves runtime.sqlite and st as they were. es is
// the workspace's open event store; the fork never appends to it.
func ForkSessionBranch(ctx context.Context, workspace string, st *SessionTree, es *EventStore, name, target string) (*SessionBranch, error) {
	if err := RefuseBranchSwitchWithPendingTerminalCommit(workspace, ""); err != nil {
		return nil, err
	}
	var branch *SessionBranch
	err := switchSessionBranch(ctx, workspace, st, func(batch *runtimeBatch) error {
		snapshotBranchState(batch, workspace, st, st.ActiveBranch)
		var err error
		branch, err = st.CreateBranch(name, target, es)
		if err != nil {
			return fmt.Errorf("failed to fork branch: %w", err)
		}
		if err := materializeCompactionBranch(batch, workspace, es, branch.ParentID, branch.ID, branch.ForkEventID); err != nil {
			return fmt.Errorf("failed to materialize compaction state for branch %q: %w", branch.ID, err)
		}
		// Snapshot the live session into the new branch so the fork starts
		// from the current state rather than a potentially stale parent
		// snapshot, then rebuild the session as the new branch's lineage.
		snapshotBranchState(batch, workspace, st, branch.ID)
		if err := rebuildSessionForBranch(batch, st, es, branch.ID); err != nil {
			return fmt.Errorf("failed to rebuild session for new branch: %w", err)
		}
		st.ActiveBranch = branch.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return branch, nil
}

// CheckoutSessionBranch activates target, committing the previous branch's
// live snapshot, the target's rebuilt session, and the active branch in one
// transaction. It returns the branch and the previously active branch ID.
func CheckoutSessionBranch(ctx context.Context, workspace string, st *SessionTree, es *EventStore, target string) (*SessionBranch, string, error) {
	resolved, err := st.ResolveCheckoutBranch(target, es)
	if err != nil {
		return nil, "", err
	}
	if err := RefuseBranchSwitchWithPendingTerminalCommit(workspace, resolved.ID); err != nil {
		return nil, "", err
	}
	previous := st.ActiveBranch
	var branch *SessionBranch
	err = switchSessionBranch(ctx, workspace, st, func(batch *runtimeBatch) error {
		snapshotBranchState(batch, workspace, st, previous)
		var err error
		if branch, err = st.CheckoutBranch(target, es); err != nil {
			return err
		}
		if err := rebuildSessionForBranch(batch, st, es, branch.ID); err != nil {
			return fmt.Errorf("failed to rebuild session for branch %q: %w", branch.ID, err)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return branch, previous, nil
}

// switchSessionBranch runs steps against a staging batch, then commits the
// staged documents with st in one transaction. On failure st is restored and
// runtime.sqlite is unchanged.
func switchSessionBranch(ctx context.Context, workspace string, st *SessionTree, steps func(*runtimeBatch) error) error {
	if st == nil {
		return fmt.Errorf("switch session branch: session tree is nil")
	}
	original := cloneSessionTree(st)
	batch := newRuntimeBatch(workspace)
	if err := steps(batch); err != nil {
		*st = *original
		return err
	}
	if err := batch.commit(ctx, st); err != nil {
		*st = *original
		return err
	}
	return nil
}

// cloneSessionTree copies st deeply enough that mutating branches, states,
// labels, or the active branch of either copy leaves the other unchanged.
func cloneSessionTree(st *SessionTree) *SessionTree {
	clone := &SessionTree{
		ActiveBranch: st.ActiveBranch,
		Branches:     make(map[string]*SessionBranch, len(st.Branches)),
		Labels:       maps.Clone(st.Labels),
		stored:       st.stored,
	}
	for id, branch := range st.Branches {
		if branch == nil {
			clone.Branches[id] = nil
			continue
		}
		copied := *branch
		copied.State = copyBranchState(branch.State)
		clone.Branches[id] = &copied
	}
	return clone
}
