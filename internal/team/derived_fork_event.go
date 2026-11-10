package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// DerivedForkCreatedPayload commits publication of a planned child. Its
// snapshot commit immediately precedes it in the same event transaction.
// Creation is a branch operation, not a model run or a workflow cycle.
type DerivedForkCreatedPayload struct {
	SchemaVersion int               `json:"schema_version"`
	Intent        DerivedForkIntent `json:"intent"`
	SnapshotID    string            `json:"snapshot_id"`
}

func decodeDerivedForkCreatedPayload(data []byte) (DerivedForkCreatedPayload, error) {
	var payload DerivedForkCreatedPayload
	if err := json.Unmarshal(data, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, fmt.Errorf("%w: %v", ErrDerivedForkIntent, err)
	}
	intent := &payload.Intent
	branch := &SessionBranch{ID: intent.BranchID, ParentID: intent.SourceBranchID, ForkEventID: intent.SourceEventID}
	if payload.SchemaVersion != derivedForkIntentVersion || !historicalGrantIdentity(payload.SnapshotID) ||
		payload.SnapshotID == intent.SourceSnapshotID || validateDerivedForkIntent(intent, branch) != nil {
		return payload, ErrDerivedForkIntent
	}
	return payload, nil
}

func decodeDerivedForkCreated(event RunEvent) (DerivedForkCreatedPayload, error) {
	payload, err := decodeDerivedForkCreatedPayload(event.Payload)
	if err != nil {
		return payload, err
	}
	if event.Type != string(EventDerivedForkCreated) || event.SchemaVersion != eventStoreSchemaVersion ||
		!historicalGrantIdentity(event.ID) || !historicalGrantIdentity(event.SessionID) || !historicalGrantIdentity(event.Timestamp) ||
		event.Actor != "workspace-versioning" || event.BranchID != payload.Intent.BranchID || event.RunID != "" ||
		event.TaskID != "" || event.Attempt != 0 || event.IdempotencyKey != derivedForkIdempotencyKey(payload.SnapshotID) {
		return payload, ErrDerivedForkIntent
	}
	return payload, nil
}

func derivedForkIdempotencyKey(snapshotID string) string { return "derived-fork:" + snapshotID }

// The planned intent and branch must already exist in the same canonical
// store. An event alone cannot attach a new-task interpretation to a branch.
func validateDerivedForkStoredBranch(ctx context.Context, q runtimestore.Queryer, event RunEvent) error {
	payload, err := decodeDerivedForkCreated(event)
	if err != nil {
		return err
	}
	branch := &SessionBranch{ID: event.BranchID}
	var data []byte
	if err := q.QueryRowContext(ctx, `SELECT parent_id, fork_event_id, derivation FROM session_branches WHERE id=?`, event.BranchID).
		Scan(&branch.ParentID, &branch.ForkEventID, &data); err != nil {
		return fmt.Errorf("%w: read planned branch: %v", ErrDerivedForkIntent, err)
	}
	intent, err := decodeDerivedForkIntent(data, branch)
	if err != nil || intent == nil || *intent != payload.Intent {
		return ErrDerivedForkIntent
	}
	var snapshotData []byte
	if err := q.QueryRowContext(ctx, `SELECT payload FROM events
WHERE type=? AND branch_id=? AND json_extract(payload, '$.snapshot_id')=?
AND sequence < COALESCE((SELECT sequence FROM events WHERE id=?), 9223372036854775807)
ORDER BY sequence LIMIT 1`, string(EventWorkspaceSnapshotCommitted), event.BranchID, payload.SnapshotID, event.ID).
		Scan(&snapshotData); err != nil {
		return fmt.Errorf("%w: child snapshot is not committed: %v", ErrDerivedForkIntent, err)
	}
	var snapshot WorkspaceSnapshotCommittedPayload
	if err := json.Unmarshal(snapshotData, &snapshot); err != nil || snapshot.ParentSnapshotID != intent.SourceSnapshotID ||
		snapshot.RootTreeHash != intent.SourceRootTreeHash || snapshot.Reason != "fork" || !snapshot.Materializable {
		return ErrDerivedForkIntent
	}
	return nil
}

func equivalentDerivedForkEvents(a, b RunEvent) bool {
	left, leftErr := decodeDerivedForkCreated(a)
	right, rightErr := decodeDerivedForkCreated(b)
	return leftErr == nil && rightErr == nil && left == right && a.Actor == b.Actor &&
		a.BranchID == b.BranchID && a.SessionID == b.SessionID && a.IdempotencyKey == b.IdempotencyKey
}
