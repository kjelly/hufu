package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BranchHead is the canonical projection of a branch's workspace lineage. It
// only ever points at a published snapshot of the same workspace and branch
// (I2), enforced by schema triggers as well as by PublishSnapshot.
type BranchHead struct {
	WorkspaceID string
	BranchID    string
	SnapshotID  SnapshotID
	Generation  int64
	UpdatedAt   time.Time
}

func getBranchHead(ctx context.Context, q queryRower, workspaceID, branchID string) (BranchHead, bool, error) {
	head := BranchHead{WorkspaceID: workspaceID, BranchID: branchID}
	var updatedAt int64
	err := q.QueryRowContext(ctx, `SELECT snapshot_id, generation, updated_at FROM branch_heads
WHERE workspace_id=? AND branch_id=?`, workspaceID, branchID).Scan(&head.SnapshotID, &head.Generation, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchHead{}, false, nil
	}
	if err != nil {
		return BranchHead{}, false, fmt.Errorf("read branch head %s/%s: %w", workspaceID, branchID, err)
	}
	head.UpdatedAt = time.UnixMilli(updatedAt)
	return head, true, nil
}

// GetBranchHead returns the head snapshot of a branch, when it has one.
func (s *Store) GetBranchHead(ctx context.Context, workspaceID, branchID string) (Snapshot, BranchHead, bool, error) {
	head, ok, err := getBranchHead(ctx, s.db, workspaceID, branchID)
	if err != nil || !ok {
		return Snapshot{}, BranchHead{}, ok, err
	}
	snapshot, err := s.GetSnapshot(ctx, head.SnapshotID)
	if err != nil {
		return Snapshot{}, BranchHead{}, false, err
	}
	return snapshot, head, true, nil
}

// ListBranchHeads returns every head of every workspace (GC roots, doctor).
func (s *Store) ListBranchHeads(ctx context.Context) ([]BranchHead, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_id, branch_id, snapshot_id, generation, updated_at
FROM branch_heads ORDER BY workspace_id, branch_id`)
	if err != nil {
		return nil, fmt.Errorf("list branch heads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var heads []BranchHead
	for rows.Next() {
		var head BranchHead
		var updatedAt int64
		if err = rows.Scan(&head.WorkspaceID, &head.BranchID, &head.SnapshotID, &head.Generation, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan branch head: %w", err)
		}
		head.UpdatedAt = time.UnixMilli(updatedAt)
		heads = append(heads, head)
	}
	return heads, rows.Err()
}

// PublishSnapshot is the only way a snapshot becomes canonical. The caller
// must already have made the snapshot's workspace_snapshot_committed event
// durable (I3); commitEventID records that event. In one transaction the
// snapshot moves pending→published and the branch head moves to it.
//
// expectedGeneration 0 means the branch must not have a head yet (the head
// row is inserted with generation 1); otherwise the head must still be at
// that generation and must be the snapshot's parent. Any mismatch is
// ErrBranchHeadConflict and nothing changes. Publishing a snapshot that is
// already published with the same commit event is a no-op.
func (s *Store) PublishSnapshot(ctx context.Context, id SnapshotID, commitEventID string, expectedGeneration int64) (BranchHead, error) {
	if err := s.requireWritable(); err != nil {
		return BranchHead{}, err
	}
	if commitEventID == "" {
		return BranchHead{}, fmt.Errorf("publish %s: commit event id is required", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var head BranchHead
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		snapshot, err := getSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		current, hasHead, err := getBranchHead(ctx, tx, snapshot.WorkspaceID, snapshot.BranchID)
		if err != nil {
			return err
		}
		if snapshot.State == StatePublished && snapshot.CommitEventID == commitEventID && hasHead && current.SnapshotID == id {
			head = current
			return nil
		}
		if snapshot.State != StatePending {
			return fmt.Errorf("%w: %s is %s", ErrWorkspaceSnapshotUnavailable, id, snapshot.State)
		}
		if err = checkHeadExpectation(snapshot, current, hasHead, expectedGeneration); err != nil {
			return err
		}
		now := s.nowMillis()
		if _, err = tx.ExecContext(ctx, `UPDATE snapshots SET state='published', commit_event_id=?, published_at=?
WHERE id=? AND state='pending'`, commitEventID, now, string(id)); err != nil {
			return fmt.Errorf("publish snapshot %s: %w", id, err)
		}
		head = BranchHead{WorkspaceID: snapshot.WorkspaceID, BranchID: snapshot.BranchID, SnapshotID: id, UpdatedAt: time.UnixMilli(now)}
		if !hasHead {
			head.Generation = 1
			_, err = tx.ExecContext(ctx, `INSERT INTO branch_heads(workspace_id, branch_id, snapshot_id, generation, updated_at)
VALUES(?,?,?,1,?)`, snapshot.WorkspaceID, snapshot.BranchID, string(id), now)
			if err != nil {
				return fmt.Errorf("%w: insert head: %v", ErrBranchHeadConflict, err)
			}
			return nil
		}
		head.Generation = current.Generation + 1
		result, err := tx.ExecContext(ctx, `UPDATE branch_heads SET snapshot_id=?, generation=generation+1, updated_at=?
WHERE workspace_id=? AND branch_id=? AND generation=?`, string(id), now, snapshot.WorkspaceID, snapshot.BranchID, expectedGeneration)
		if err != nil {
			return fmt.Errorf("advance head: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return fmt.Errorf("%w: head of %s moved", ErrBranchHeadConflict, snapshot.BranchID)
		}
		return nil
	})
	if err != nil {
		return BranchHead{}, err
	}
	return head, nil
}

func checkHeadExpectation(snapshot Snapshot, current BranchHead, hasHead bool, expectedGeneration int64) error {
	switch {
	case expectedGeneration == 0 && hasHead:
		return fmt.Errorf("%w: %s already has a head", ErrBranchHeadConflict, snapshot.BranchID)
	case expectedGeneration > 0 && !hasHead:
		return fmt.Errorf("%w: %s has no head at generation %d", ErrBranchHeadConflict, snapshot.BranchID, expectedGeneration)
	case hasHead && current.Generation != expectedGeneration:
		return fmt.Errorf("%w: %s is at generation %d, not %d", ErrBranchHeadConflict, snapshot.BranchID, current.Generation, expectedGeneration)
	case hasHead && snapshot.Parent != current.SnapshotID:
		return fmt.Errorf("%w: %s does not descend from head %s", ErrBranchHeadConflict, snapshot.ID, current.SnapshotID)
	}
	return nil
}

// OrphanSnapshot abandons a pending snapshot that never became canonical.
func (s *Store) OrphanSnapshot(ctx context.Context, id SnapshotID) error {
	if err := s.requireWritable(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, "UPDATE snapshots SET state='orphaned' WHERE id=? AND state='pending'", string(id))
	if err != nil {
		return fmt.Errorf("orphan snapshot %s: %w", id, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("%w: %s is not pending", ErrWorkspaceSnapshotUnavailable, id)
	}
	return nil
}

// DeriveRequest creates a snapshot node that reuses an existing tree.
type DeriveRequest struct {
	WorkspaceID string
	BranchID    string
	// Parent is the node the new snapshot descends from.
	Parent SnapshotID
	// Source provides the root tree (and its statistics).
	Source SnapshotID
	Reason SnapshotReason
	RunID  string
}

// DeriveSnapshot creates a pending node whose tree is Source's tree. No
// bytes are copied: a fork is O(1) in CAS storage.
func (s *Store) DeriveSnapshot(ctx context.Context, req DeriveRequest) (Snapshot, error) {
	if err := s.requireWritable(); err != nil {
		return Snapshot{}, err
	}
	if req.WorkspaceID == "" || req.BranchID == "" || !req.Reason.Valid() {
		return Snapshot{}, fmt.Errorf("derive snapshot: workspace, branch, and a valid reason are required")
	}
	source, err := s.GetSnapshot(ctx, req.Source)
	if err != nil {
		return Snapshot{}, err
	}
	if source.State != StatePublished && source.State != StatePending {
		return Snapshot{}, fmt.Errorf("%w: source %s is %s", ErrWorkspaceSnapshotUnavailable, req.Source, source.State)
	}
	if err = requirePublishedParent(ctx, s.db, req.Parent); err != nil {
		return Snapshot{}, err
	}
	id, err := s.newSnapshotID()
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		ID: id, WorkspaceID: req.WorkspaceID, BranchID: req.BranchID, Parent: req.Parent,
		RootTreeHash: source.RootTreeHash, ManifestDigest: source.ManifestDigest, FileCount: source.FileCount,
		LogicalBytes: source.LogicalBytes, Materializable: source.Materializable, Reason: req.Reason,
		RunID: req.RunID, State: StatePending, CreatedAt: s.now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.withTx(ctx, func(tx *sql.Tx) error { return insertSnapshot(ctx, tx, snapshot) }); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// AttachBranch creates a branch's first node from base (a fork): the new
// node's parent and tree are both base.
func (s *Store) AttachBranch(ctx context.Context, workspaceID, branchID string, base SnapshotID, reason SnapshotReason) (Snapshot, error) {
	return s.DeriveSnapshot(ctx, DeriveRequest{WorkspaceID: workspaceID, BranchID: branchID, Parent: base, Source: base, Reason: reason})
}
