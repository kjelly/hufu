package versionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OperationKind names a version operation.
type OperationKind string

const (
	OperationCapture     OperationKind = "capture"
	OperationFork        OperationKind = "fork"
	OperationCheckout    OperationKind = "checkout"
	OperationRestore     OperationKind = "restore"
	OperationMaterialize OperationKind = "materialize"
	OperationAdopt       OperationKind = "adopt"
	OperationDowngrade   OperationKind = "downgrade"
	OperationGC          OperationKind = "gc"
)

// OperationState is the recovery checkpoint of an operation.
type OperationState string

const (
	OperationPrepared          OperationState = "prepared"
	OperationObjectsWritten    OperationState = "objects_written"
	OperationEventCommitted    OperationState = "event_committed"
	OperationMaterializing     OperationState = "materializing"
	OperationMaterialized      OperationState = "materialized"
	OperationProjectionUpdated OperationState = "projection_updated"
	OperationCompleted         OperationState = "completed"
	OperationFailed            OperationState = "failed"
)

// Operation is one row of the operations table.
type Operation struct {
	ID             OperationID
	WorkspaceID    string
	BranchID       string
	Kind           OperationKind
	State          OperationState
	FromSnapshotID SnapshotID
	ToSnapshotID   SnapshotID
	IdempotencyKey string
	TargetBranchID string
	DetailCode     string
	StartedAt      time.Time
	UpdatedAt      time.Time
}

// Terminal reports whether the operation needs no recovery.
func (o Operation) Terminal() bool {
	return o.State == OperationCompleted || o.State == OperationFailed
}

const operationColumns = `id, workspace_id, branch_id, kind, state, from_snapshot_id, to_snapshot_id,
idempotency_key, target_branch_id, detail_code, started_at, updated_at`

func scanOperation(row rowScanner) (Operation, error) {
	var (
		op                            Operation
		branch, from, to, key, target sql.NullString
		kind, state                   string
		startedAt, updatedAt          int64
	)
	if err := row.Scan(&op.ID, &op.WorkspaceID, &branch, &kind, &state, &from, &to, &key, &target, &op.DetailCode, &startedAt, &updatedAt); err != nil {
		return Operation{}, err
	}
	op.BranchID, op.Kind, op.State = branch.String, OperationKind(kind), OperationState(state)
	op.FromSnapshotID, op.ToSnapshotID = SnapshotID(from.String), SnapshotID(to.String)
	op.IdempotencyKey, op.TargetBranchID = key.String, target.String
	op.StartedAt, op.UpdatedAt = time.UnixMilli(startedAt), time.UnixMilli(updatedAt)
	return op, nil
}

// CreateOperation records a new operation in state prepared (unless State is
// set) and returns it with its generated ID.
func (s *Store) CreateOperation(ctx context.Context, op Operation) (Operation, error) {
	if err := s.requireWritable(); err != nil {
		return Operation{}, err
	}
	if op.WorkspaceID == "" || op.Kind == "" {
		return Operation{}, fmt.Errorf("create operation: workspace and kind are required")
	}
	id, err := s.newOperationID()
	if err != nil {
		return Operation{}, err
	}
	op.ID = id
	if op.State == "" {
		op.State = OperationPrepared
	}
	now := s.now()
	op.StartedAt, op.UpdatedAt = now, now
	_, err = s.db.ExecContext(ctx, `INSERT INTO operations(`+operationColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(op.ID), op.WorkspaceID, nullString(op.BranchID), string(op.Kind), string(op.State),
		nullString(string(op.FromSnapshotID)), nullString(string(op.ToSnapshotID)), nullString(op.IdempotencyKey),
		nullString(op.TargetBranchID), op.DetailCode, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Operation{}, fmt.Errorf("create %s operation: %w", op.Kind, err)
	}
	return op, nil
}

// OperationUpdate changes an operation's state and, when non-empty, its
// target snapshot and detail code.
type OperationUpdate struct {
	State        OperationState
	ToSnapshotID SnapshotID
	DetailCode   string
}

// UpdateOperation applies update to a non-terminal operation.
func (s *Store) UpdateOperation(ctx context.Context, id OperationID, update OperationUpdate) error {
	if err := s.requireWritable(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET state=?,
to_snapshot_id=COALESCE(NULLIF(?, ''), to_snapshot_id), detail_code=COALESCE(NULLIF(?, ''), detail_code), updated_at=?
WHERE id=? AND state NOT IN ('completed','failed')`,
		string(update.State), string(update.ToSnapshotID), update.DetailCode, s.nowMillis(), string(id))
	if err != nil {
		return fmt.Errorf("update operation %s: %w", id, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("update operation %s: not found or already terminal", id)
	}
	return nil
}

// GetOperation returns one operation.
func (s *Store) GetOperation(ctx context.Context, id OperationID) (Operation, error) {
	op, err := scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM operations WHERE id=?", string(id)))
	if err != nil {
		return Operation{}, fmt.Errorf("read operation %s: %w", id, err)
	}
	return op, nil
}

// IncompleteOperations lists operations that recovery must finish, oldest first.
func (s *Store) IncompleteOperations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+operationColumns+" FROM operations WHERE state NOT IN ('completed','failed') ORDER BY started_at, id")
	if err != nil {
		return nil, fmt.Errorf("list incomplete operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ops []Operation
	for rows.Next() {
		op, scanErr := scanOperation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan operation: %w", scanErr)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func (s *Store) newOperationID() (OperationID, error) {
	id, err := newID(s.random, "wop")
	return OperationID(id), err
}

func (s *Store) nowMillis() int64 { return s.now().UnixMilli() }
