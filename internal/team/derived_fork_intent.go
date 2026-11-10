package team

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kjelly/hufu/internal/runtimestore"
)

const derivedForkIntentVersion = 1

var ErrDerivedForkIntent = errors.New("invalid derived fork intent")

// DerivedForkIntent is immutable native branch metadata. Saving it with the
// branch prevents a crash before snapshot publication from turning a new-task
// fork into an ordinary stateful fork. It contains no inherited instructions,
// result bodies, or authority to execute the source's tasks.
type DerivedForkIntent struct {
	SchemaVersion           int    `json:"schema_version"`
	WorkspaceID             string `json:"workspace_id"`
	Workspace               string `json:"workspace"`
	Team                    string `json:"team"`
	BranchID                string `json:"branch_id"`
	SourceBranchID          string `json:"source_branch_id"`
	SourceEventID           string `json:"source_event_id"`
	SourceEventHash         string `json:"source_event_hash"`
	SourceSnapshotID        string `json:"source_snapshot_id"`
	SourceSnapshotEventID   string `json:"source_snapshot_event_id"`
	SourceSnapshotEventHash string `json:"source_snapshot_event_hash"`
	SourceRootTreeHash      string `json:"source_root_tree_hash"`
	CaptureOrigin           string `json:"capture_origin"`
	CaptureSnapshotID       string `json:"capture_snapshot_id,omitempty"`
	CaptureEventID          string `json:"capture_event_id,omitempty"`
	CaptureEventHash        string `json:"capture_event_hash,omitempty"`
	TaskIDFloor             int    `json:"task_id_floor"`
}

func (intent *DerivedForkIntent) clone() *DerivedForkIntent {
	if intent == nil {
		return nil
	}
	return new(*intent)
}

func validateDerivedForkIntent(intent *DerivedForkIntent, branch *SessionBranch) error {
	if intent == nil {
		return nil
	}
	if intent.SchemaVersion != derivedForkIntentVersion || branch == nil || intent.BranchID != branch.ID ||
		intent.SourceBranchID != branch.ParentID || intent.SourceEventID != branch.ForkEventID ||
		intent.BranchID == intent.SourceBranchID || intent.TaskIDFloor < 0 ||
		!filepath.IsAbs(intent.Workspace) || filepath.Clean(intent.Workspace) != intent.Workspace ||
		!historicalGrantCanonicalName(intent.Team) {
		return ErrDerivedForkIntent
	}
	for _, identity := range []string{intent.WorkspaceID, intent.BranchID, intent.SourceBranchID,
		intent.SourceEventID, intent.SourceSnapshotID, intent.SourceSnapshotEventID} {
		if !historicalGrantIdentity(identity) {
			return ErrDerivedForkIntent
		}
	}
	for _, hash := range []string{intent.SourceEventHash, intent.SourceSnapshotEventHash, intent.SourceRootTreeHash} {
		if !historicalGrantSHA256(hash) {
			return ErrDerivedForkIntent
		}
	}
	switch intent.CaptureOrigin {
	case "known":
		if !historicalGrantIdentity(intent.CaptureSnapshotID) || !historicalGrantIdentity(intent.CaptureEventID) || !historicalGrantSHA256(intent.CaptureEventHash) {
			return ErrDerivedForkIntent
		}
	case "unavailable":
		if intent.CaptureSnapshotID != "" || intent.CaptureEventID != "" || intent.CaptureEventHash != "" {
			return ErrDerivedForkIntent
		}
	default:
		return ErrDerivedForkIntent
	}
	return nil
}

func decodeDerivedForkIntent(data []byte, branch *SessionBranch) (*DerivedForkIntent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var intent *DerivedForkIntent
	if err := json.Unmarshal(data, &intent, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDerivedForkIntent, err)
	}
	if intent == nil {
		return nil, ErrDerivedForkIntent
	}
	if err := validateDerivedForkIntent(intent, branch); err != nil {
		return nil, err
	}
	return intent, nil
}

func encodeDerivedForkIntent(intent *DerivedForkIntent) ([]byte, error) {
	if intent == nil {
		return nil, nil
	}
	return json.Marshal(intent, json.Deterministic(true))
}

// Branch metadata is initialized only on insertion. Neither a checkpoint
// refresh nor a caller replacing a SessionTree can remove or attach an intent
// to an existing branch. The surrounding tree transaction includes this read.
func validateStoredDerivedForkIntent(ctx context.Context, q runtimestore.Queryer, id string, intent *DerivedForkIntent) error {
	var stored []byte
	err := q.QueryRowContext(ctx, `SELECT derivation FROM session_branches WHERE id=?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(stored) == 0 && intent == nil {
		return nil
	}
	if len(stored) == 0 || intent == nil {
		return ErrDerivedForkIntent
	}
	var previous DerivedForkIntent
	if err := json.Unmarshal(stored, &previous, json.RejectUnknownMembers(true)); err != nil || previous != *intent {
		return ErrDerivedForkIntent
	}
	return nil
}
