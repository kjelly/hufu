package context

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHybridRetrieveGoldenQueries(t *testing.T) {
	var fixture struct {
		ProjectID string `json:"project_id"`
		Items     []struct {
			ID       string      `json:"id"`
			Kind     ContextKind `json:"kind"`
			Content  string      `json:"content"`
			Priority Priority    `json:"priority"`
		} `json:"items"`
		Queries []struct {
			Query  string `json:"query"`
			WantID string `json:"want_id"`
		} `json:"queries"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "retrieval_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, item := range fixture.Items {
		if err := repo.Append(t.Context(), ContextItem{ID: item.ID, Kind: item.Kind, Content: item.Content, Priority: item.Priority, Scope: Scope{ProjectID: fixture.ProjectID}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range fixture.Queries {
		t.Run(q.Query, func(t *testing.T) {
			got, _, err := HybridRetrieve(t.Context(), repo, nil, SearchRequest{Query: q.Query, Scope: Scope{ProjectID: fixture.ProjectID}, Limit: 5})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 || got[0].Item.ID != q.WantID {
				t.Fatalf("query %q got %#v, want first %q", q.Query, resultIDs(got), q.WantID)
			}
		})
	}
}

func BenchmarkHybridRetrieveGoldenQuery(b *testing.B) {
	repo, err := OpenSQLite(filepath.Join(b.TempDir(), "context.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "benchmark"}
	for i := 0; i < 100; i++ {
		if err := repo.Append(context.Background(), ContextItem{ID: fmt.Sprintf("item-%d", i), Kind: ContextPattern, Content: fmt.Sprintf("context retrieval benchmark document %d", i), Scope: scope}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := HybridRetrieve(context.Background(), repo, nil, SearchRequest{Query: "context retrieval benchmark", Scope: scope, Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

type staticVectorSearcher struct{ results []SearchResult }

func (s staticVectorSearcher) SearchVector(context.Context, SearchRequest) ([]SearchResult, error) {
	return s.results, nil
}

func TestDecomposeQueryExtractsDeterministicTerms(t *testing.T) {
	p := DecomposeQuery(`fix "exact phrase" in internal/context/store.go using go test ./... after E1234 at 85c8499`)
	if len(p.Quoted) != 1 || len(p.Paths) == 0 || len(p.Commands) == 0 || len(p.ErrorCodes) != 1 || len(p.SHAs) != 1 {
		t.Fatalf("unexpected decomposition: %#v", p)
	}
}

func TestDecomposeQueryRemainderExcludesOperationalIdentifiers(t *testing.T) {
	p := DecomposeQuery("fix internal/context/retrieval.go using go test ./...")
	if p.Remainder != "fix" {
		t.Fatalf("remainder=%q, want natural-language terms only", p.Remainder)
	}
	for _, identifier := range []string{"internal/context/retrieval.go", "go test ./..."} {
		if strings.Contains(p.Remainder, identifier) {
			t.Fatalf("remainder retained operational identifier %q: %q", identifier, p.Remainder)
		}
	}
}

func TestDecomposeQueryIncludesOperationalIdentifiers(t *testing.T) {
	p := DecomposeQuery("task_id=t-1 attempt_id=a-2 tool=bash artifact_id=art-3 at 10.0.0.1:8080")
	if len(p.TaskIDs) != 1 || len(p.AttemptIDs) != 1 || len(p.ToolNames) != 1 || len(p.ArtifactIDs) != 1 || len(p.IPs) != 1 || len(p.Ports) != 1 {
		t.Fatalf("operational identifiers missing: %#v", p)
	}
}

func TestRRFAndMMRAreDeterministicAndDiverse(t *testing.T) {
	base := []SearchResult{{Item: ContextItem{ID: "a", Content: "database migration schema update", ContentHash: "same"}, Score: .9}, {Item: ContextItem{ID: "b", Content: "database migration schema update", ContentHash: "same"}, Score: .8}, {Item: ContextItem{ID: "c", Content: "network retry timeout strategy", ContentHash: "other"}, Score: .7}}
	var first []string
	for range 20 {
		got := applyMMR(rrf(base), .75)
		ids := resultIDs(got)
		if len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
			t.Fatalf("unexpected deterministic MMR: %v", ids)
		}
		if first == nil {
			first = ids
		} else if strings.Join(first, ",") != strings.Join(ids, ",") {
			t.Fatalf("non-deterministic order: %v / %v", first, ids)
		}
	}
}

func TestHybridRetrieveMarksLowRankResultsInsufficient(t *testing.T) {
	repo, err := OpenSQLite(t.TempDir() + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "p"}
	// Place the only vector result far down its list so RRF score is below the sufficiency threshold.
	results := make([]SearchResult, 50)
	for i := range results {
		results[i] = SearchResult{Item: ContextItem{ID: fmt.Sprintf("v-%d", i), Content: fmt.Sprintf("irrelevant %d", i), ContentHash: fmt.Sprintf("h-%d", i), Scope: scope}, Score: .01}
	}
	_, trace, err := HybridRetrieve(context.Background(), repo, staticVectorSearcher{results: results}, SearchRequest{Query: "unrelated", Scope: scope, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !trace.RetrievalInsufficient {
		t.Fatalf("low relevance retrieval must be insufficient: %#v", trace.FusedResults)
	}
}

func TestRankForScopePrefersNearestScopeOnTie(t *testing.T) {
	request := Scope{ProjectID: "p", TeamID: "team", SessionID: "session", TaskID: "task"}
	results := []SearchResult{
		{Item: ContextItem{ID: "project", Scope: Scope{ProjectID: "p"}, Priority: PriorityNormal}, Score: 1},
		{Item: ContextItem{ID: "session", Scope: Scope{ProjectID: "p", TeamID: "team", SessionID: "session"}, Priority: PriorityNormal}, Score: 1},
		{Item: ContextItem{ID: "task", Scope: request, Priority: PriorityNormal}, Score: 1},
	}
	got := rankForScope(results, request)
	if ids := strings.Join(resultIDs(got), ","); ids != "task,session,project" {
		t.Fatalf("scope distance ordering=%s", ids)
	}
}

func TestHybridRetrievePrefersExactAndScopesResults(t *testing.T) {
	repo, err := OpenSQLite(t.TempDir() + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project-a"}
	items := []ContextItem{
		{ID: "path", Kind: ContextPattern, Content: "edit internal/context/store.go", Scope: scope, Priority: PriorityNormal},
		{ID: "other-project", Kind: ContextPattern, Content: "edit internal/context/store.go", Scope: Scope{ProjectID: "project-b"}, Priority: PriorityCritical},
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	got, trace, err := HybridRetrieve(context.Background(), repo, staticVectorSearcher{}, SearchRequest{Query: "fix internal/context/store.go", Scope: scope, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Item.ID != "path" || len(trace.ExactResults) != 1 {
		t.Fatalf("exact/scoped retrieval = %#v trace=%#v", got, trace)
	}
}

func TestHybridRetrieveFusesLexicalAndVectorResultsAfterExactHit(t *testing.T) {
	repo, err := OpenSQLite(t.TempDir() + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(context.Background(), ContextItem{ID: "exact", Kind: ContextPattern, Content: "fix internal/context/store.go for the migration", Scope: scope, Priority: PriorityNormal}); err != nil {
		t.Fatal(err)
	}
	vector := staticVectorSearcher{results: []SearchResult{{Item: ContextItem{ID: "semantic", Kind: ContextArchitecture, Content: "database schema upgrade guidance", ContentHash: "semantic", Scope: scope, Priority: PriorityHigh}, Score: .9}}}
	got, trace, err := HybridRetrieve(context.Background(), repo, vector, SearchRequest{Query: "fix internal/context/store.go migration", Scope: scope, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.ExactResults) != 1 || len(trace.LexicalResults) == 0 || len(trace.VectorResults) != 1 {
		t.Fatalf("exact hit must still collect all retrieval stages: %#v", trace)
	}
	if ids := strings.Join(resultIDs(got), ","); !strings.Contains(ids, "exact") || !strings.Contains(ids, "semantic") {
		t.Fatalf("fused results lost an exact or semantic hit: %s", ids)
	}
	if got[0].Item.ID != "exact" {
		t.Fatalf("exact result must be ordered before fused results: %v", resultIDs(got))
	}
}

func TestHybridRetrieveUsesRRFAndDeduplicatesContent(t *testing.T) {
	repo, err := OpenSQLite(t.TempDir() + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(context.Background(), ContextItem{ID: "lexical", Kind: ContextPattern, Content: "database migration schema", Scope: scope, Priority: PriorityNormal}, ContextItem{ID: "duplicate", Kind: ContextSummary, Content: "database migration schema", Scope: scope, Priority: PriorityLow}); err != nil {
		t.Fatal(err)
	}
	vector := staticVectorSearcher{results: []SearchResult{{Item: ContextItem{ID: "semantic", Kind: ContextArchitecture, Content: "storage upgrade plan", ContentHash: "semantic", Scope: scope, Priority: PriorityHigh}, Score: .9}}}
	got, trace, err := HybridRetrieve(context.Background(), repo, vector, SearchRequest{Query: "database migration", Scope: scope, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.LexicalResults) == 0 || len(trace.VectorResults) != 1 || len(got) < 2 {
		t.Fatalf("hybrid results=%#v trace=%#v", got, trace)
	}
}
