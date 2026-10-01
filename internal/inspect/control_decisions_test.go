package inspect

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// writeControlDecisionBranches records one observation on "main" and one on
// an independent root branch, as a later --new run does, and makes the new
// branch active.
func writeControlDecisionBranches(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-old", "session-old")
	if err != nil {
		t.Fatal(err)
	}
	observation := func(value string) []byte {
		payload, err := json.Marshal(map[string]any{"version": 1, "point": "path-reviewer", "mode": "shadow", "applied": "legacy", "status": "decided",
			"value": value, "confidence": 0.95, "accepted": true, "threshold": 0.9, "duration_ms": 100, "legacy": "true", "agree": value == "true"})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	for _, step := range []struct {
		branch, runID, value string
	}{
		{branch: "main", runID: "run-old", value: "true"},
		{branch: "session-new", runID: "run-new", value: "false"},
	} {
		store.SetBranchID(step.branch)
		if _, err := store.AppendPersisted(team.RunEvent{Type: "run_started", RunID: step.runID, Actor: "coordinator", Payload: []byte(`{"goal":"g"}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendPersisted(team.RunEvent{Type: string(team.EventControlDecisionObserved), RunID: step.runID, Actor: "coder", Payload: observation(step.value)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree := team.NewSessionTree()
	tree.Branches["session-new"] = &team.SessionBranch{ID: "session-new", Name: "session-new", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	tree.ActiveBranch = "session-new"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestInspectControlDecisionsAllBranches(t *testing.T) {
	workspace := writeControlDecisionBranches(t)
	tests := []struct {
		name     string
		query    InspectQuery
		scope    string
		calls    int
		agreed   int
		branchID string
	}{
		{name: "active branch only", query: InspectQuery{Workspace: workspace}, scope: "branch", calls: 1, agreed: 0, branchID: "session-new"},
		{name: "every branch", query: InspectQuery{Workspace: workspace, AllBranches: true}, scope: "all_branches", calls: 2, agreed: 1},
		{name: "every branch, one run", query: InspectQuery{Workspace: workspace, AllBranches: true, RunID: "run-old"}, scope: "all_branches", calls: 1, agreed: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := InspectControlDecisions(t.Context(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			data := envelope.Data.(ControlDecisionsData)
			if data.Scope != test.scope || len(data.Summaries) != 1 || data.Summaries[0].Calls != test.calls || data.Summaries[0].Agreed != test.agreed {
				t.Fatalf("data = %#v", data)
			}
			if envelope.Query.BranchID != test.branchID || envelope.Query.AllBranches != test.query.AllBranches {
				t.Fatalf("query = %#v", envelope.Query)
			}
		})
	}
	for name, query := range map[string]InspectQuery{
		"with a branch":   {Workspace: workspace, AllBranches: true, BranchID: "main"},
		"on another kind": {Workspace: workspace, AllBranches: true},
	} {
		kind := KindControlDecisions
		if name == "on another kind" {
			kind = KindCost
		}
		if err := query.Validate(kind); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: Validate = %v, want ErrInvalidQuery", name, err)
		}
	}
}
