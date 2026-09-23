package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const snapshotColumns = `id, workspace_id, branch_id, parent_snapshot_id, root_tree_hash, manifest_digest,
file_count, logical_bytes, materializable, reason, run_id, task_id, attempt, anchor_event_id,
commit_event_id, state, created_at, published_at, pruned_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func millisTime(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return time.UnixMilli(value.Int64)
}

func scanSnapshot(row rowScanner) (Snapshot, error) {
	var (
		snapshot                              Snapshot
		parent, runID, taskID, anchor, commit sql.NullString
		attempt, publishedAt, prunedAt        sql.NullInt64
		createdAt                             int64
		materializable                        int
		reason, state                         string
	)
	err := row.Scan(&snapshot.ID, &snapshot.WorkspaceID, &snapshot.BranchID, &parent, &snapshot.RootTreeHash,
		&snapshot.ManifestDigest, &snapshot.FileCount, &snapshot.LogicalBytes, &materializable, &reason,
		&runID, &taskID, &attempt, &anchor, &commit, &state, &createdAt, &publishedAt, &prunedAt)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Parent = SnapshotID(parent.String)
	snapshot.Materializable = materializable == 1
	snapshot.Reason = SnapshotReason(reason)
	snapshot.RunID = runID.String
	snapshot.TaskID = taskID.String
	snapshot.Attempt = int(attempt.Int64)
	snapshot.AnchorEventID = anchor.String
	snapshot.CommitEventID = commit.String
	snapshot.State = SnapshotState(state)
	snapshot.CreatedAt = time.UnixMilli(createdAt)
	snapshot.PublishedAt = millisTime(publishedAt)
	snapshot.PrunedAt = millisTime(prunedAt)
	return snapshot, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getSnapshot(ctx context.Context, q queryRower, id SnapshotID) (Snapshot, error) {
	snapshot, err := scanSnapshot(q.QueryRowContext(ctx, "SELECT "+snapshotColumns+" FROM snapshots WHERE id=?", string(id)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrSnapshotNotFound, id)
		}
		return Snapshot{}, fmt.Errorf("read workspace snapshot %s: %w", id, err)
	}
	return snapshot, nil
}

// GetSnapshot returns one snapshot row.
func (s *Store) GetSnapshot(ctx context.Context, id SnapshotID) (Snapshot, error) {
	return getSnapshot(ctx, s.db, id)
}

func insertSnapshot(ctx context.Context, tx *sql.Tx, snapshot Snapshot) error {
	materializable := 0
	if snapshot.Materializable {
		materializable = 1
	}
	var attempt sql.NullInt64
	if snapshot.Attempt > 0 {
		attempt = sql.NullInt64{Int64: int64(snapshot.Attempt), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO snapshots(id, workspace_id, branch_id, parent_snapshot_id,
root_tree_hash, manifest_digest, file_count, logical_bytes, materializable, reason, run_id, task_id,
attempt, anchor_event_id, state, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(snapshot.ID), snapshot.WorkspaceID, snapshot.BranchID, nullString(string(snapshot.Parent)),
		snapshot.RootTreeHash, snapshot.ManifestDigest, snapshot.FileCount, snapshot.LogicalBytes, materializable,
		string(snapshot.Reason), nullString(snapshot.RunID), nullString(snapshot.TaskID), attempt,
		nullString(snapshot.AnchorEventID), string(StatePending), snapshot.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert workspace snapshot: %w", err)
	}
	return nil
}

// requirePublishedParent checks that parent (when set) is a published node.
func requirePublishedParent(ctx context.Context, q queryRower, parent SnapshotID) error {
	if parent == "" {
		return nil
	}
	snapshot, err := getSnapshot(ctx, q, parent)
	if err != nil {
		return err
	}
	if snapshot.State != StatePublished {
		return fmt.Errorf("%w: parent %s is %s", ErrWorkspaceSnapshotUnavailable, parent, snapshot.State)
	}
	return nil
}

// SnapshotFilter narrows ListSnapshots; empty fields match everything.
type SnapshotFilter struct {
	WorkspaceID string
	BranchID    string
	State       SnapshotState
}

// ListSnapshots returns matching snapshots, oldest first.
func (s *Store) ListSnapshots(ctx context.Context, filter SnapshotFilter) ([]Snapshot, error) {
	query := "SELECT " + snapshotColumns + " FROM snapshots WHERE (?='' OR workspace_id=?) AND (?='' OR branch_id=?) AND (?='' OR state=?) ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, query, filter.WorkspaceID, filter.WorkspaceID, filter.BranchID, filter.BranchID, string(filter.State), string(filter.State))
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var snapshots []Snapshot
	for rows.Next() {
		snapshot, scanErr := scanSnapshot(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan snapshot: %w", scanErr)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}
