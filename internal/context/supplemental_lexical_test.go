package context

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSupplementalLexicalJoinPreservesScoresAndAuthorization(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := Scope{ProjectID: "project", TeamID: "team"}
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := WithEvaluationTime(t.Context(), now)
	var items []ContextItem
	for index := range 80 {
		item := ContextItem{ID: fmt.Sprintf("noise-%02d", index), Kind: ContextPattern, Scope: scope, Content: fmt.Sprintf("migration sentinel migration sentinel migration sentinel noise-%02d", index)}
		switch index % 4 {
		case 1:
			item.Scope.TeamID = "foreign"
		case 2:
			item.Lifecycle = LifecycleCandidate
		case 3:
			item.ExpiresAt = new(now.Add(-time.Hour))
		}
		items = append(items, item)
	}
	items = append(items,
		ContextItem{ID: "allowed-first", Kind: ContextPattern, Scope: scope, Content: "migration sentinel migration sentinel"},
		ContextItem{ID: "allowed-second", Kind: ContextPattern, Scope: scope, Content: "migration sentinel"})
	if err := repo.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}
	// A customized default must not change the explicit legacy BM25 contract.
	if _, err := repo.db.ExecContext(ctx, "INSERT INTO context_items_fts(context_items_fts,rank) VALUES('rank','bm25(5.0,0.0,0.0,0.0)')"); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 64} {
		for _, query := range []string{"migration sentinel", "migration /src/router.go", "純中文"} {
			request := SearchRequest{Query: query, Scope: scope, Limit: limit, AllowedItemIDs: []string{"allowed-first", "allowed-second", "noise-01", "noise-02", "noise-03"}}
			legacy, err := repo.SearchLexical(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.SearchSupplementalLexical(ctx, request)
			if err != nil || !reflect.DeepEqual(legacy, got) {
				t.Fatalf("fixed join changed canonical BM25 results: limit=%d query=%q err=%v", limit, query, err)
			}
			if query == "migration sentinel" && len(got) != min(2, limit) {
				t.Fatal("allow-set applied after limit or unauthorized result admitted")
			}
		}
	}
	request := SearchRequest{Query: "migration", Scope: scope, Limit: 64, AllowedItemIDs: []string{}}
	if rows, err := repo.SearchSupplementalLexical(ctx, request); err != nil || len(rows) != 0 {
		t.Fatal("empty allow-set broadened authorization")
	}
	request.AllowedItemIDs = nil
	if _, err := repo.SearchSupplementalLexical(ctx, request); err == nil {
		t.Fatal("unrestricted supplemental search accepted")
	}
}

func TestSupplementalLexicalScopeVisibilityParity(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	leaf := Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "worker", TaskID: "task", AttemptID: "attempt"}
	scopes := []Scope{leaf}
	ancestor := leaf
	for _, field := range []*string{&ancestor.AttemptID, &ancestor.TaskID, &ancestor.AgentID, &ancestor.BranchID, &ancestor.SessionID, &ancestor.TeamID} {
		*field = ""
		scopes = append(scopes, ancestor)
	}
	for _, change := range []func(*Scope){
		func(s *Scope) { s.ProjectID = "foreign" },
		func(s *Scope) { s.TeamID = "foreign" },
		func(s *Scope) { s.SessionID = "foreign" },
		func(s *Scope) { s.BranchID = "foreign" },
		func(s *Scope) { s.AgentID = "foreign" },
		func(s *Scope) { s.TaskID = "foreign" },
		func(s *Scope) { s.AttemptID = "foreign" },
	} {
		foreign := leaf
		change(&foreign)
		scopes = append(scopes, foreign)
	}
	var allowed []string
	for index, scope := range scopes {
		id := fmt.Sprintf("scope-%d", index)
		allowed = append(allowed, id)
		if err := repo.Append(t.Context(), ContextItem{ID: id, Scope: scope, Kind: ContextPattern, Content: "scope sentinel " + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, visibility := range []ScopeVisibility{"", VisibilityAncestors, VisibilityExact, VisibilitySubtree} {
		for _, scope := range scopes[:7] {
			request := SearchRequest{Query: "scope sentinel", Scope: scope, Visibility: visibility, Limit: 64, AllowedItemIDs: allowed}
			legacy, err := repo.SearchLexical(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.SearchSupplementalLexical(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			asMap := func(rows []SearchResult) map[string]SearchResult {
				result := make(map[string]SearchResult, len(rows))
				for _, row := range rows {
					result[row.Item.ID] = row
				}
				return result
			}
			if !reflect.DeepEqual(asMap(legacy), asMap(got)) {
				t.Fatalf("scope authorization changed: visibility=%s scope=%+v", visibility, scope)
			}
		}
	}
}
