package operator

import (
	"strings"
	"testing"
)

func pendingDerivedSnapshot() OperatorSnapshot {
	snapshot := testSnapshot()
	snapshot.Scope.RunID, snapshot.Scope.SelectionSource = "", "derived_fork"
	snapshot.Scope.BranchID = "child"
	snapshot.Scope.SessionID, snapshot.Scope.TeamName = "session", "review"
	snapshot.Activity = ActivityView{State: ActivityWaitingInput}
	snapshot.Outcome = OutcomeView{CompletionState: "awaiting_new_request"}
	snapshot.Freshness.EventHash = strings.Repeat("a", 64)
	snapshot.DerivedFork = &DerivedForkView{
		BranchID: "child", EventID: "creation", EventHash: strings.Repeat("a", 64), SnapshotID: "child-snapshot",
		SourceBranchID: "main", SourceRunID: "parent-run", SourceRunMembership: "visible",
		SourceEventID: "source", SourceEventHash: strings.Repeat("b", 64), SourceSnapshotID: "parent-snapshot",
		SourceRootTreeHash: strings.Repeat("c", 64), CaptureOrigin: "unavailable", AwaitingNewRequest: true,
	}
	return snapshot
}

func TestDerivedSnapshotRequiresVerifiedPendingScope(t *testing.T) {
	for _, variant := range []string{"valid", "missing_provenance", "parent_run", "finished", "goal_satisfied", "membership", "partial_capture", "first_cycle", "hash"} {
		t.Run(variant, func(t *testing.T) {
			snapshot := pendingDerivedSnapshot()
			switch variant {
			case "missing_provenance":
				snapshot.DerivedFork = nil
			case "parent_run":
				snapshot.Scope.RunID = "parent-run"
			case "finished":
				snapshot.Activity.State = ActivityFinished
			case "goal_satisfied":
				snapshot.Outcome.GoalSatisfied = new(true)
			case "membership":
				snapshot.DerivedFork.SourceRunMembership = "unknown"
			case "partial_capture":
				snapshot.DerivedFork.CaptureOrigin = "known"
			case "first_cycle":
				snapshot.DerivedFork.FirstCycleID = "not-yet-admitted"
			case "hash":
				snapshot.DerivedFork.SourceEventHash = "invalid"
			}
			_, err := FinalizeSnapshot(snapshot)
			if (err == nil) != (variant == "valid") {
				t.Fatalf("pending scope %s: %v", variant, err)
			}
		})
	}
}

func TestNormalizeSnapshotDetachesDerivedProvenance(t *testing.T) {
	source := pendingDerivedSnapshot()
	copy := NormalizeSnapshot(source)
	copy.DerivedFork.SourceRunID = "changed"
	if source.DerivedFork.SourceRunID != "parent-run" {
		t.Fatal("normalization shares mutable provenance with its caller")
	}
}
