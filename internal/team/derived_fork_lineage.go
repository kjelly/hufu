package team

import (
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// A branch column alone is an unpublished plan. A native event alone cannot
// attach a new interpretation to a branch. Checked readers require both,
// authenticate the source prefix, and retain the original ancestor identity
// when an ordinary fork inherits a pending derived lineage.
func validateDerivedForkLineage(events []RunEvent, tree *SessionTree, target string) error {
	if tree == nil {
		if slices.ContainsFunc(events, func(event RunEvent) bool { return event.Type == string(EventDerivedForkCreated) }) {
			return ErrDerivedForkIntent
		}
		return nil
	}
	target = effectiveBranchID(target)
	required := tree.Branches[target] != nil && tree.Branches[target].Derivation != nil
	if !required {
		required = slices.ContainsFunc(events, func(event RunEvent) bool {
			branch := tree.Branches[effectiveEventBranchID(event)]
			return event.Type == string(EventDerivedForkCreated) || branch != nil && branch.Derivation != nil
		})
	}
	if !required {
		return nil
	}
	seen := make(map[string]bool)
	needs := make(map[string]bool)
	ownBeforeCreation := make(map[string]bool)
	if branch := tree.Branches[target]; branch != nil && branch.Derivation != nil {
		needs[target] = true
	}
	for index, event := range events {
		branchID := effectiveEventBranchID(event)
		branch := tree.Branches[branchID]
		if branch != nil && branch.Derivation != nil {
			needs[branchID] = true
		}
		if event.Type == string(EventDerivedForkCreated) {
			payload, err := decodeDerivedForkCreated(event)
			if err != nil {
				return err
			}
			if seen[branchID] || ownBeforeCreation[branchID] || branch == nil || branch.Derivation == nil || *branch.Derivation != payload.Intent ||
				!historicalGrantIdentity(event.ID) || !historicalGrantSHA256(event.Hash) {
				return ErrDerivedForkIntent
			}
			if err := validateDerivedForkIntent(branch.Derivation, branch); err != nil {
				return err
			}
			if err := validateDerivedForkPublication(events, index, payload); err != nil {
				return err
			}
			if err := validateDerivedForkSources(events[:index], payload.Intent); err != nil {
				return err
			}
			seen[branchID] = true
		} else if event.Type != string(EventWorkspaceSnapshotCommitted) && !seen[branchID] {
			ownBeforeCreation[branchID] = true
		}
	}
	for branchID := range needs {
		if !seen[branchID] {
			return fmt.Errorf("%w: derived branch %s has no committed initialization; inspect workspace recovery", ErrDerivedForkIntent, branchID)
		}
	}
	return nil
}

func validateDerivedForkPublication(events []RunEvent, index int, payload DerivedForkCreatedPayload) error {
	if index == 0 {
		return ErrDerivedForkIntent
	}
	commit := events[index-1]
	snapshot, ok := decodeWorkspaceSnapshotEvent(commit)
	if !ok || commit.Actor != "workspace-versioning" || commit.BranchID != payload.Intent.BranchID || events[index].PreviousID != commit.ID ||
		snapshot.SnapshotID != payload.SnapshotID || snapshot.ParentSnapshotID != payload.Intent.SourceSnapshotID ||
		snapshot.RootTreeHash != payload.Intent.SourceRootTreeHash || !snapshot.Materializable || snapshot.Reason != string(versionstore.SnapshotFork) {
		return ErrDerivedForkIntent
	}
	return nil
}

func validateDerivedForkSources(prefix []RunEvent, intent DerivedForkIntent) error {
	end := slices.IndexFunc(prefix, func(event RunEvent) bool { return event.ID == intent.SourceEventID })
	if end < 0 || prefix[end].Hash != intent.SourceEventHash {
		return ErrDerivedForkIntent
	}
	// The child publication has no task IDs. Use the prefix ending at the
	// exact source cutoff, so later child events cannot raise its frozen floor.
	prefix = prefix[:end+1]
	if workflowTaskIDHighWater(prefix) != intent.TaskIDFloor {
		return ErrDerivedForkIntent
	}
	snapshots := make(map[string]RunEvent)
	var source RunEvent
	for _, event := range prefix {
		if payload, ok := decodeWorkspaceSnapshotEvent(event); ok {
			snapshots[payload.SnapshotID] = event
		}
		if event.ID == intent.SourceSnapshotEventID {
			source = event
		}
	}
	snapshot, ok := decodeWorkspaceSnapshotEvent(source)
	if source.Actor != "workspace-versioning" || source.Hash != intent.SourceSnapshotEventHash || !ok || snapshot.SnapshotID != intent.SourceSnapshotID ||
		snapshot.RootTreeHash != intent.SourceRootTreeHash || !snapshot.Materializable {
		return ErrDerivedForkIntent
	}
	capture, known, err := derivedForkCaptureEvent(snapshots, source)
	if err != nil {
		return err
	}
	if !known {
		if intent.CaptureOrigin != "unavailable" {
			return ErrDerivedForkIntent
		}
		return nil
	}
	captured, _ := decodeWorkspaceSnapshotEvent(capture)
	if intent.CaptureOrigin != "known" || intent.CaptureSnapshotID != captured.SnapshotID ||
		intent.CaptureEventID != capture.ID || intent.CaptureEventHash != capture.Hash {
		return ErrDerivedForkIntent
	}
	return nil
}

func derivedForkCaptureEvent(snapshots map[string]RunEvent, source RunEvent) (RunEvent, bool, error) {
	seen := make(map[string]bool)
	for {
		payload, ok := decodeWorkspaceSnapshotEvent(source)
		if !ok || seen[payload.SnapshotID] {
			return RunEvent{}, false, ErrDerivedForkIntent
		}
		seen[payload.SnapshotID] = true
		switch versionstore.SnapshotReason(payload.Reason) {
		case versionstore.SnapshotRestore:
			return RunEvent{}, false, nil
		case versionstore.SnapshotFork:
			parent, exists := snapshots[payload.ParentSnapshotID]
			parentPayload, valid := decodeWorkspaceSnapshotEvent(parent)
			if !exists || !valid || parentPayload.RootTreeHash != payload.RootTreeHash {
				return RunEvent{}, false, ErrDerivedForkIntent
			}
			source = parent
		default:
			return source, true, nil
		}
	}
}
