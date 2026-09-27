package context

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRepositoryQueryIDsFilterBeforeLimit(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, item := range []ContextItem{
		{ID: "high", Kind: ContextDecision, Content: "high", Priority: PriorityCritical, Scope: Scope{ProjectID: "p"}},
		{ID: "low", Kind: ContextDecision, Content: "low", Priority: PriorityBackground, Scope: Scope{ProjectID: "p"}},
		{ID: "other", Kind: ContextDecision, Content: "other", Scope: Scope{ProjectID: "q"}},
	} {
		if err := repo.Append(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{name: "nil is unrestricted", ids: nil, want: []string{"high"}},
		{name: "empty matches nothing", ids: []string{}, want: nil},
		{name: "id wins over limit order", ids: []string{"low"}, want: []string{"low"}},
		{name: "scope still applies", ids: []string{"other"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := repo.Query(context.Background(), RepositoryQuery{Scope: Scope{ProjectID: "p"}, IDs: tc.ids, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, item := range items {
				got = append(got, item.ID)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}
