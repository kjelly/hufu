package inspect

import (
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestHistoricalReadBudgetAppliesToExactLineage(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	fork := appendHistoricalTask(t, store, "run-first", "1", "inherited")
	large := appendHistoricalTask(t, store, "run-first", "2", strings.Repeat("old content", 1024))
	tree := team.NewSessionTree()
	tree.Branches["child"] = &team.SessionBranch{ID: "child", ParentID: "main", ForkEventID: fork.ID}
	tree.Branches["fresh"] = &team.SessionBranch{ID: "fresh"}
	tree.ActiveBranch = "fresh"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	current := appendHistoricalFixture(t, store, team.RunEvent{BranchID: "fresh", RunID: "run-current", Type: "run_finished"}, map[string]any{"team": "review", "outcome": "completed", "response": "current report"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"fresh", "child"} {
		lineage, _, teams, err := historicalLineage(t.Context(), HistoricalResultScope{Workspace: workspace, Branch: branch}, 100, 4096)
		if err != nil {
			t.Fatalf("branch %s: %v", branch, err)
		}
		if !teams["review"] || lineage.GlobalEventCount != 6 || len(lineage.GlobalEvents) != 6 {
			t.Fatalf("global identity or provenance lost: %#v, %v", lineage, teams)
		}
		for _, indexed := range lineage.Events {
			if indexed.Event.ID == large.ID || len(indexed.Event.Payload) == 0 {
				t.Fatalf("incorrect selected content: %#v", indexed)
			}
		}
		if branch == "fresh" && (len(lineage.Events) != 1 || lineage.Events[0].Event.ID != current.ID || lineage.Events[0].Ordinal != 6) {
			t.Fatalf("fresh branch widened or ordinal changed: %#v", lineage.Events)
		}
		if branch == "child" && (len(lineage.Events) != 3 || lineage.Events[2].Event.ID != fork.ID) {
			t.Fatalf("fork cutoff changed: %#v", lineage.Events)
		}
	}
	for _, scope := range []HistoricalResultScope{
		{Workspace: workspace, Branch: "main"},
		{Workspace: workspace, AllBranches: true},
	} {
		lineage, _, _, err := historicalLineage(t.Context(), scope, 100, 4096)
		if !errors.Is(err, ErrInvalidQuery) || len(lineage.Events) != 0 {
			t.Fatalf("over-budget content returned: %#v, %v", lineage, err)
		}
	}
	if _, _, _, err := historicalLineage(t.Context(), HistoricalResultScope{Workspace: workspace, Branch: "fresh"}, 5, 4096); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("global header budget not enforced: %v", err)
	}
	// Corruption outside the selected branch must still invalidate the read.
	runtime, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Exec(t.Context(), "UPDATE events SET payload = '{}' WHERE id = ?", large.ID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	lineage, _, _, err := historicalLineage(t.Context(), HistoricalResultScope{Workspace: workspace, Branch: "fresh"}, 100, 4096)
	if !errors.Is(err, ErrIntegrity) || len(lineage.Events) != 0 {
		t.Fatalf("corrupt sibling ignored: %#v, %v", lineage, err)
	}
}
