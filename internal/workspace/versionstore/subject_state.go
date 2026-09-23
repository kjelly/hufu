package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SubjectState is the per-subject-root state that belongs to no single
// branch: the persistent root binding, the mode floor, the diagnostic
// "materialized" projection, and the recovery / deferred-checkpoint markers.
type SubjectState struct {
	SubjectKey              string
	SubjectRoot             string
	ModeFloor               Mode
	MaterializedWorkspaceID string
	MaterializedBranchID    string
	MaterializedSnapshotID  SnapshotID
	RecoveryRequired        bool
	RecoveryCode            string
	RecoveryDetail          string
	CheckpointDeferred      bool
	Generation              int64
	UpdatedAt               time.Time
}

const subjectColumns = `subject_key, subject_root, mode_floor, materialized_workspace_id, materialized_branch_id,
materialized_snapshot_id, recovery_required, recovery_code, recovery_detail, checkpoint_deferred, generation, updated_at`

func scanSubjectState(row rowScanner) (SubjectState, error) {
	var (
		state                         SubjectState
		floor                         string
		workspaceID, branchID, snapID sql.NullString
		recovery, deferred            int
		updatedAt                     int64
	)
	err := row.Scan(&state.SubjectKey, &state.SubjectRoot, &floor, &workspaceID, &branchID, &snapID,
		&recovery, &state.RecoveryCode, &state.RecoveryDetail, &deferred, &state.Generation, &updatedAt)
	if err != nil {
		return SubjectState{}, err
	}
	state.ModeFloor = Mode(floor)
	state.MaterializedWorkspaceID, state.MaterializedBranchID = workspaceID.String, branchID.String
	state.MaterializedSnapshotID = SnapshotID(snapID.String)
	state.RecoveryRequired, state.CheckpointDeferred = recovery == 1, deferred == 1
	state.UpdatedAt = time.UnixMilli(updatedAt)
	return state, nil
}

// GetSubjectState returns the row for canonicalRoot, if it exists.
func (s *Store) GetSubjectState(ctx context.Context, canonicalRoot string) (SubjectState, bool, error) {
	state, err := scanSubjectState(s.db.QueryRowContext(ctx, "SELECT "+subjectColumns+" FROM subject_state WHERE subject_key=?", SubjectKey(canonicalRoot)))
	if errors.Is(err, sql.ErrNoRows) {
		return SubjectState{}, false, nil
	}
	if err != nil {
		return SubjectState{}, false, fmt.Errorf("read subject state: %w", err)
	}
	if state.SubjectRoot != canonicalRoot {
		return SubjectState{}, false, fmt.Errorf("%w: subject key bound to %q, not %q", ErrVersionedWorkspaceUnresolved, state.SubjectRoot, canonicalRoot)
	}
	return state, true, nil
}

// EnsureSubjectState binds canonicalRoot on first use and enforces the mode
// floor: a mode below the recorded floor fails with ErrModeDowngradeRefused,
// and a higher mode raises the floor (the floor only moves down through
// Downgrade).
func (s *Store) EnsureSubjectState(ctx context.Context, canonicalRoot string, mode Mode) (SubjectState, error) {
	if err := s.requireWritable(); err != nil {
		return SubjectState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok, err := s.GetSubjectState(ctx, canonicalRoot)
	if err != nil {
		return SubjectState{}, err
	}
	now := s.nowMillis()
	if !ok {
		_, err = s.db.ExecContext(ctx, `INSERT INTO subject_state(subject_key, subject_root, mode_floor, generation, updated_at)
VALUES(?,?,?,1,?)`, SubjectKey(canonicalRoot), canonicalRoot, string(mode), now)
		if err != nil {
			return SubjectState{}, fmt.Errorf("bind subject root: %w", err)
		}
		state, _, err = s.GetSubjectState(ctx, canonicalRoot)
		return state, err
	}
	if mode.Below(state.ModeFloor) {
		return SubjectState{}, fmt.Errorf("%w: configured %s, floor %s", ErrModeDowngradeRefused, mode, state.ModeFloor)
	}
	if state.ModeFloor.Below(mode) {
		if err = s.updateSubjectLocked(ctx, canonicalRoot, func(next *SubjectState) { next.ModeFloor = mode }); err != nil {
			return SubjectState{}, err
		}
		state, _, err = s.GetSubjectState(ctx, canonicalRoot)
	}
	return state, err
}

// UpdateSubjectState applies mutate with optimistic generation control.
func (s *Store) UpdateSubjectState(ctx context.Context, canonicalRoot string, mutate func(*SubjectState)) error {
	if err := s.requireWritable(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateSubjectLocked(ctx, canonicalRoot, mutate)
}

func (s *Store) updateSubjectLocked(ctx context.Context, canonicalRoot string, mutate func(*SubjectState)) error {
	state, ok, err := s.GetSubjectState(ctx, canonicalRoot)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: subject root %q is not bound", ErrVersionedWorkspaceUnresolved, canonicalRoot)
	}
	next := state
	mutate(&next)
	boolInt := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	result, err := s.db.ExecContext(ctx, `UPDATE subject_state SET mode_floor=?, materialized_workspace_id=?,
materialized_branch_id=?, materialized_snapshot_id=?, recovery_required=?, recovery_code=?, recovery_detail=?,
checkpoint_deferred=?, generation=generation+1, updated_at=? WHERE subject_key=? AND generation=?`,
		string(next.ModeFloor), nullString(next.MaterializedWorkspaceID), nullString(next.MaterializedBranchID),
		nullString(string(next.MaterializedSnapshotID)), boolInt(next.RecoveryRequired), next.RecoveryCode,
		next.RecoveryDetail, boolInt(next.CheckpointDeferred), s.nowMillis(), state.SubjectKey, state.Generation)
	if err != nil {
		return fmt.Errorf("update subject state: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("update subject state: generation %d moved concurrently", state.Generation)
	}
	return nil
}
