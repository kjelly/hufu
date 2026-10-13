package context

import (
	"slices"
	"testing"
)

func TestTeamIDs(t *testing.T) {
	repository := openProjectionTestRepository(t)
	for _, item := range []ContextItem{
		{ID: "a", Kind: ContextDecision, Content: "a", Scope: Scope{ProjectID: "p", TeamID: "research"}},
		{ID: "b", Kind: ContextDecision, Content: "b", Scope: Scope{ProjectID: "p", TeamID: "review"}},
		{ID: "c", Kind: ContextDecision, Content: "c", Scope: Scope{ProjectID: "p", TeamID: "research", SessionID: "s"}},
		{ID: "d", Kind: ContextDecision, Content: "d", Scope: Scope{ProjectID: "p"}},
		{ID: "e", Kind: ContextDecision, Content: "e", Scope: Scope{ProjectID: "other", TeamID: "ops"}},
	} {
		if err := repository.Append(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		project string
		want    []string
	}{
		{project: "p", want: []string{"research", "review"}},
		{project: "other", want: []string{"ops"}},
		{project: "missing", want: nil},
	}
	for _, tc := range tests {
		got, err := repository.TeamIDs(t.Context(), tc.project)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("TeamIDs(%q) = %v, want %v", tc.project, got, tc.want)
		}
	}
}
