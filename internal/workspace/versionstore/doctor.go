package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// DoctorIssue is one finding of Doctor (§29).
type DoctorIssue struct {
	Code        string     `json:"code"`
	Severity    string     `json:"severity"` // "error" or "warning"
	WorkspaceID string     `json:"workspace_id,omitempty"`
	BranchID    string     `json:"branch_id,omitempty"`
	SnapshotID  SnapshotID `json:"snapshot_id,omitempty"`
	Detail      string     `json:"detail"`
	// Repairable marks findings --repair can fix deterministically.
	Repairable bool `json:"repairable"`
}

const (
	severityError   = "error"
	severityWarning = "warning"
	staleTempAge    = time.Hour
)

// Doctor runs the store-level integrity checks: heads point at published
// snapshots, published snapshots carry a commit event, parent chains end,
// every non-pruned snapshot's objects exist and hash to their names (I4),
// and it reports incomplete operations, recovery / deferred markers, and
// stale temp files. It never modifies anything. Event-lineage and lock-owner
// checks need the event store and process table, so callers add them.
func (s *Store) Doctor(ctx context.Context) ([]DoctorIssue, error) {
	var issues []DoctorIssue
	add := func(issue DoctorIssue) { issues = append(issues, issue) }
	heads, err := s.ListBranchHeads(ctx)
	if err != nil {
		return nil, err
	}
	for _, head := range heads {
		snapshot, getErr := s.GetSnapshot(ctx, head.SnapshotID)
		if getErr != nil || snapshot.State != StatePublished || snapshot.BranchID != head.BranchID {
			add(DoctorIssue{Code: "head_invalid", Severity: severityError, WorkspaceID: head.WorkspaceID, BranchID: head.BranchID, SnapshotID: head.SnapshotID,
				Detail: fmt.Sprintf("head does not reference a published snapshot of the branch (%v)", getErr)})
		}
	}
	snapshots, err := s.ListSnapshots(ctx, SnapshotFilter{})
	if err != nil {
		return nil, err
	}
	byID := make(map[SnapshotID]Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		byID[snapshot.ID] = snapshot
	}
	verified := make(map[objectKey]error)
	for _, snapshot := range snapshots {
		issues = append(issues, s.checkSnapshot(ctx, snapshot, byID, verified)...)
	}
	ops, err := s.IncompleteOperations(ctx)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		add(DoctorIssue{Code: "incomplete_operation", Severity: severityWarning, WorkspaceID: op.WorkspaceID, BranchID: op.BranchID,
			Detail: fmt.Sprintf("%s operation %s is %s; the next mutating command completes or fails it", op.Kind, op.ID, op.State), Repairable: true})
	}
	markerIssues, err := s.subjectMarkerIssues(ctx)
	if err != nil {
		return nil, err
	}
	issues = append(issues, markerIssues...)
	stale, err := s.staleTempFiles(s.now().Add(-staleTempAge))
	if err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		add(DoctorIssue{Code: "stale_temp_files", Severity: severityWarning, Detail: fmt.Sprintf("%d temp file(s) older than %s", len(stale), staleTempAge), Repairable: true})
	}
	return issues, nil
}

func (s *Store) checkSnapshot(ctx context.Context, snapshot Snapshot, byID map[SnapshotID]Snapshot, verified map[objectKey]error) []DoctorIssue {
	var issues []DoctorIssue
	base := DoctorIssue{WorkspaceID: snapshot.WorkspaceID, BranchID: snapshot.BranchID, SnapshotID: snapshot.ID}
	if snapshot.State == StatePublished && snapshot.CommitEventID == "" {
		issue := base
		issue.Code, issue.Severity, issue.Detail = "published_without_commit_event", severityError, "published snapshot has no commit event"
		issues = append(issues, issue)
	}
	if snapshot.State == StatePending {
		issue := base
		issue.Code, issue.Severity, issue.Detail, issue.Repairable = "pending_snapshot", severityWarning, "pending snapshot awaits recovery", true
		issues = append(issues, issue)
	}
	seen := map[SnapshotID]bool{snapshot.ID: true}
	for parent := snapshot.Parent; parent != ""; parent = byID[parent].Parent {
		if _, ok := byID[parent]; !ok {
			issue := base
			issue.Code, issue.Severity, issue.Detail = "parent_missing", severityError, fmt.Sprintf("parent %s does not exist", parent)
			issues = append(issues, issue)
			break
		}
		if seen[parent] {
			issue := base
			issue.Code, issue.Severity, issue.Detail = "parent_cycle", severityError, fmt.Sprintf("parent chain loops at %s", parent)
			issues = append(issues, issue)
			break
		}
		seen[parent] = true
	}
	if snapshot.State == StatePruned || snapshot.State == StateOrphaned {
		return issues
	}
	if err := s.verifyTreeMemo(ctx, snapshot.RootTreeHash, verified); err != nil {
		issue := base
		issue.Code, issue.Severity, issue.Detail = "cas_integrity", severityError, err.Error()
		issues = append(issues, issue)
	}
	return issues
}

// verifyTreeMemo is Verify with shared memoization across snapshots.
func (s *Store) verifyTreeMemo(ctx context.Context, hash string, verified map[objectKey]error) error {
	key := objectKey{kind: ObjectTree, hash: hash}
	if err, done := verified[key]; done {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := s.readTree(hash)
	if err == nil {
		for _, entry := range tree.Entries {
			if err = s.verifyEntryMemo(ctx, entry, verified); err != nil {
				break
			}
		}
	}
	verified[key] = err
	return err
}

func (s *Store) verifyEntryMemo(ctx context.Context, entry TreeEntry, verified map[objectKey]error) error {
	if entry.Kind == EntryTree {
		return s.verifyTreeMemo(ctx, entry.ObjectHash, verified)
	}
	kind, ok := entry.Kind.objectKind()
	if !ok {
		return fmt.Errorf("%w: entry %q has unknown kind", ErrInvalidTree, entry.Name)
	}
	key := objectKey{kind: kind, hash: entry.ObjectHash}
	if err, done := verified[key]; done {
		return err
	}
	var err error
	switch kind {
	case ObjectBlob:
		var size int64
		if size, err = s.hashObjectFile(ObjectBlob, entry.ObjectHash); err == nil && size != entry.Size {
			err = fmt.Errorf("%w: blob %s has %d bytes, tree records %d", ErrCASCorrupt, entry.ObjectHash, size, entry.Size)
		}
	case ObjectLink:
		var target string
		if target, err = s.readLink(entry.ObjectHash); err == nil && int64(len(target)) != entry.Size {
			err = fmt.Errorf("%w: link %s length mismatch", ErrCASCorrupt, entry.ObjectHash)
		}
	}
	if err != nil {
		err = fmt.Errorf("%q: %w", entry.Name, err)
	}
	verified[key] = err
	return err
}

func (s *Store) subjectMarkerIssues(ctx context.Context) ([]DoctorIssue, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+subjectColumns+" FROM subject_state ORDER BY subject_key")
	if err != nil {
		return nil, fmt.Errorf("read subject state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var issues []DoctorIssue
	for rows.Next() {
		state, scanErr := scanSubjectState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if state.RecoveryRequired {
			issues = append(issues, DoctorIssue{Code: "recovery_required", Severity: severityWarning,
				Detail: fmt.Sprintf("%s: %s (%s); use `hufu workspace version restore <head>` or `adopt`", state.SubjectRoot, state.RecoveryCode, state.RecoveryDetail)})
		}
		if state.CheckpointDeferred {
			issues = append(issues, DoctorIssue{Code: "checkpoint_deferred", Severity: severityWarning,
				Detail: state.SubjectRoot + ": the last run's files were not checkpointed; the next run admission records them"})
		}
	}
	return issues, rows.Err()
}

// RemoveStaleTempFiles deletes temp files older than an hour (doctor --repair).
func (s *Store) RemoveStaleTempFiles() (int, error) {
	stale, err := s.staleTempFiles(s.now().Add(-staleTempAge))
	if err != nil {
		return 0, err
	}
	for _, path := range stale {
		if removeErr := removeIfExists(path); removeErr != nil {
			return 0, removeErr
		}
	}
	return len(stale), nil
}

// RepairBranchHead points a branch's head at a published snapshot of that
// branch chosen from the canonical event lineage (doctor --repair).
func (s *Store) RepairBranchHead(ctx context.Context, workspaceID, branchID string, id SnapshotID) error {
	if err := s.requireWritable(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		snapshot, err := getSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if snapshot.State != StatePublished || snapshot.WorkspaceID != workspaceID || snapshot.BranchID != branchID {
			return fmt.Errorf("%w: %s is not a published snapshot of %s/%s", ErrWorkspaceSnapshotUnavailable, id, workspaceID, branchID)
		}
		now := s.nowMillis()
		result, err := tx.ExecContext(ctx, `UPDATE branch_heads SET snapshot_id=?, generation=generation+1, updated_at=?
WHERE workspace_id=? AND branch_id=?`, string(id), now, workspaceID, branchID)
		if err != nil {
			return fmt.Errorf("repair head: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO branch_heads(workspace_id, branch_id, snapshot_id, generation, updated_at) VALUES(?,?,?,1,?)`,
			workspaceID, branchID, string(id), now)
		if err != nil {
			return fmt.Errorf("repair head: %w", err)
		}
		return nil
	})
}

// Downgrade explicitly lowers the mode floor of a subject root (§31) and
// records the operation. It is the only way the floor moves down.
func (s *Store) Downgrade(ctx context.Context, canonicalRoot, workspaceID string, mode Mode) error {
	state, ok, err := s.GetSubjectState(ctx, canonicalRoot)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: subject root %q has no recorded mode", ErrVersionedWorkspaceUnresolved, canonicalRoot)
	}
	if !mode.Below(state.ModeFloor) {
		return fmt.Errorf("downgrade: %s is not below the current floor %s", mode, state.ModeFloor)
	}
	op, err := s.CreateOperation(ctx, Operation{WorkspaceID: workspaceID, Kind: OperationDowngrade, DetailCode: string(state.ModeFloor) + "->" + string(mode)})
	if err != nil {
		return err
	}
	if err = s.UpdateSubjectState(ctx, canonicalRoot, func(next *SubjectState) { next.ModeFloor = mode }); err != nil {
		return err
	}
	return s.UpdateOperation(ctx, op.ID, OperationUpdate{State: OperationCompleted})
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
