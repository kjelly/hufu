package operator

import (
	"encoding/hex"
	"fmt"
)

// DerivedForkView contains only provenance already committed by the native
// fork. Snapshot references describe saved evidence, not present availability
// of filesystem objects or permission to resume the source's tasks.
type DerivedForkView struct {
	BranchID            string `json:"branch_id"`
	EventID             string `json:"event_id"`
	EventHash           string `json:"event_hash"`
	SnapshotID          string `json:"snapshot_id"`
	SourceBranchID      string `json:"source_branch_id"`
	SourceRunID         string `json:"source_run_id,omitempty"`
	SourceRunMembership string `json:"source_run_membership"`
	SourceEventID       string `json:"source_event_id"`
	SourceEventHash     string `json:"source_event_hash"`
	SourceSnapshotID    string `json:"source_snapshot_id"`
	SourceRootTreeHash  string `json:"source_root_tree_hash"`
	CaptureOrigin       string `json:"capture_origin"`
	CaptureSnapshotID   string `json:"capture_snapshot_id,omitempty"`
	CaptureEventID      string `json:"capture_event_id,omitempty"`
	CaptureEventHash    string `json:"capture_event_hash,omitempty"`
	AwaitingNewRequest  bool   `json:"awaiting_new_request"`
	FirstCycleID        string `json:"first_cycle_id,omitempty"`
}

func cloneDerivedForkView(view *DerivedForkView) *DerivedForkView {
	if view == nil {
		return nil
	}
	return new(*view)
}

func validateDerivedForkView(view *DerivedForkView) error {
	if view == nil {
		return nil
	}
	if view.BranchID == view.SourceBranchID || view.SnapshotID == view.SourceSnapshotID || view.EventID == view.SourceEventID {
		return fmt.Errorf("derived fork source aliases its child")
	}
	for _, value := range []string{view.BranchID, view.EventID, view.SnapshotID, view.SourceBranchID, view.SourceEventID, view.SourceSnapshotID} {
		if value == "" {
			return fmt.Errorf("derived fork provenance is incomplete")
		}
	}
	for _, value := range []string{view.EventHash, view.SourceEventHash, view.SourceRootTreeHash} {
		if !derivedForkHash(value) {
			return fmt.Errorf("derived fork provenance hash is invalid")
		}
	}
	if view.SourceRunMembership != "visible" && view.SourceRunMembership != "unavailable" ||
		(view.SourceRunID != "") != (view.SourceRunMembership == "visible") ||
		view.AwaitingNewRequest && view.FirstCycleID != "" || !view.AwaitingNewRequest && view.FirstCycleID == "" {
		return fmt.Errorf("derived fork request or source membership is invalid")
	}
	switch view.CaptureOrigin {
	case "known":
		if view.CaptureSnapshotID == "" || view.CaptureEventID == "" || !derivedForkHash(view.CaptureEventHash) {
			return fmt.Errorf("derived fork capture provenance is incomplete")
		}
	case "unavailable":
		if view.CaptureSnapshotID != "" || view.CaptureEventID != "" || view.CaptureEventHash != "" {
			return fmt.Errorf("unavailable capture provenance contains evidence")
		}
	default:
		return fmt.Errorf("derived fork capture origin is invalid")
	}
	return nil
}

func derivedForkHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func pendingDerivedScope(snapshot OperatorSnapshot) bool {
	return snapshot.DerivedFork != nil && snapshot.DerivedFork.AwaitingNewRequest &&
		snapshot.Scope.BindingStatus == "verified" &&
		snapshot.Scope.SessionID != "" && snapshot.Scope.TeamName != "" &&
		snapshot.Scope.SelectionSource == "derived_fork" && snapshot.Scope.RunID == "" && snapshot.Scope.InvocationID == "" &&
		snapshot.Activity.State == ActivityWaitingInput && snapshot.Outcome.RunOutcome == "" && snapshot.Outcome.GoalSatisfied == nil &&
		snapshot.Freshness.EventID != "" && derivedForkHash(snapshot.Freshness.EventHash)
}
