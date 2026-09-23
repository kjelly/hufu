package versionstore

import (
	"context"
	"fmt"
)

// CommitEventLookup reports whether a durable workspace_snapshot_committed
// event exists for a snapshot, and its event ID. It is supplied by the
// runtime that owns the event store; versionstore never reads events itself.
type CommitEventLookup func(ctx context.Context, id SnapshotID) (eventID string, found bool, err error)

// RecoveryActionKind is what recovery did to one snapshot.
type RecoveryActionKind string

const (
	RecoveryPublished RecoveryActionKind = "published"
	RecoveryOrphaned  RecoveryActionKind = "orphaned"
)

// RecoveryAction records one deterministic repair.
type RecoveryAction struct {
	Kind       RecoveryActionKind
	SnapshotID SnapshotID
	BranchID   string
	EventID    string
}

// RecoverPendingSnapshots settles every pending snapshot of workspaceID
// (§16.2): a snapshot whose commit event is durable is published and its
// branch head repaired; one without a durable event is orphaned. Recovery
// never appends or replays events (I13) — the live filesystem is still
// there, and the next admission or drift capture records it again.
func (s *Store) RecoverPendingSnapshots(ctx context.Context, workspaceID string, lookup CommitEventLookup) ([]RecoveryAction, error) {
	if lookup == nil {
		return nil, fmt.Errorf("recover pending snapshots: lookup is required")
	}
	pending, err := s.ListSnapshots(ctx, SnapshotFilter{WorkspaceID: workspaceID, State: StatePending})
	if err != nil {
		return nil, err
	}
	var actions []RecoveryAction
	for _, snapshot := range pending {
		eventID, found, lookupErr := lookup(ctx, snapshot.ID)
		if lookupErr != nil {
			return actions, fmt.Errorf("look up commit event for %s: %w", snapshot.ID, lookupErr)
		}
		if !found {
			if err = s.OrphanSnapshot(ctx, snapshot.ID); err != nil {
				return actions, err
			}
			actions = append(actions, RecoveryAction{Kind: RecoveryOrphaned, SnapshotID: snapshot.ID, BranchID: snapshot.BranchID})
			continue
		}
		_, head, hasHead, headErr := s.GetBranchHead(ctx, snapshot.WorkspaceID, snapshot.BranchID)
		if headErr != nil {
			return actions, headErr
		}
		expected := int64(0)
		if hasHead {
			expected = head.Generation
		}
		if _, err = s.PublishSnapshot(ctx, snapshot.ID, eventID, expected); err != nil {
			return actions, fmt.Errorf("repair head for %s: %w", snapshot.ID, err)
		}
		actions = append(actions, RecoveryAction{Kind: RecoveryPublished, SnapshotID: snapshot.ID, BranchID: snapshot.BranchID, EventID: eventID})
	}
	return actions, nil
}
