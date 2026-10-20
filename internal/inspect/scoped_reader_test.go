package inspect

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func fixtureReadLimits() ReadLimits {
	return ReadLimits{MaxEvents: 100000, MaxPayloadBytes: 32 << 20, MaxArtifactBytes: 32 << 20}
}

func TestScopedReaderReadOnlyAndFixedScope(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "session-evidence", BranchID: "main"}
	reader, err := NewScopedReader(t.Context(), query, fixtureReadLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotInspectWorkspace(t, fixture.workspace)
	var observations []*ScopedObservation
	for _, kind := range []Kind{KindRun, KindRoutes, KindEconomics} {
		result, err := reader.Inspect(t.Context(), kind, "")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		observations = append(observations, result)
		if result.SourceHead != reader.anchor {
			t.Fatalf("wrong event head: %#v", result)
		}
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("scoped queries changed workspace")
	}
	if _, err := reader.Inspect(t.Context(), KindRoutes, "sibling-task"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign task accepted: %v", err)
	}
	if _, err := reader.Inspect(t.Context(), KindRoutes, "../task"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("path accepted as task: %v", err)
	}
	tree, err := team.LoadSessionTree(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree.Branches["other"] = &team.SessionBranch{ID: "other", Name: "other", ParentID: "main", ForkEventID: reader.anchor.EventID}
	tree.ActiveBranch = "other"
	if err := team.SaveSessionTree(fixture.workspace, tree); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []Kind{KindRun, KindRoutes, KindEconomics} {
		result, err := reader.Inspect(t.Context(), kind, "")
		if err != nil || !reflect.DeepEqual(result, observations[i]) {
			t.Fatalf("%s followed checkout: %v", kind, err)
		}
	}
}

func TestScopedReaderRejectsLimitsAndBranchAliases(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "session-evidence", BranchID: "main"}
	limits := fixtureReadLimits()
	limits.MaxEvents = 1
	if _, err := NewScopedReader(t.Context(), query, limits); !errors.Is(err, team.ErrReadLimit) {
		t.Fatalf("startup bypassed event limit: %v", err)
	}
	limits = fixtureReadLimits()
	limits.MaxArtifactBytes = 1
	reader, err := NewScopedReader(t.Context(), query, limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []Kind{KindRun, KindRoutes, KindEconomics} {
		result, err := reader.Inspect(t.Context(), kind, "")
		if result != nil || !errors.Is(err, team.ErrReadLimit) {
			t.Fatalf("%s converted read limit to successful diagnostic: result=%v err=%v", kind, result, err)
		}
	}
	tree, err := team.LoadSessionTree(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree.Branches["main"].Name = "alias"
	if err := team.SaveSessionTree(fixture.workspace, tree); err != nil {
		t.Fatal(err)
	}
	query.BranchID = "alias"
	if _, err := NewScopedReader(t.Context(), query, fixtureReadLimits()); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("mutable branch name accepted: %v", err)
	}
	query.BranchID = ""
	if _, err := NewScopedReader(t.Context(), query, fixtureReadLimits()); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("implicit active branch accepted: %v", err)
	}
}
