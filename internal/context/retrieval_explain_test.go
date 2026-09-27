package context

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
)

type fixedVector struct{ results []SearchResult }

func (v fixedVector) SearchVector(context.Context, SearchRequest) ([]SearchResult, error) {
	return v.results, nil
}

func TestRetrievalObserverDoesNotChangeResults(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	rng := rand.New(rand.NewSource(7))
	words := []string{"sqlite", "schema", "migration", "worker", "coordinator", "retry", "vector", "lexical", "cache", "budget"}
	var items []ContextItem
	for i := range 80 {
		content := ""
		for range 3 + rng.Intn(5) {
			content += words[rng.Intn(len(words))] + " "
		}
		if i%9 == 0 {
			content = "sqlite schema migration duplicate"
		}
		item := ContextItem{ID: fmt.Sprintf("item-%02d", i), Kind: ContextDecision, Content: content + fmt.Sprint(i%9 != 0), Scope: Scope{ProjectID: "p"}, Priority: Priority(10 * (i % 5))}
		if i%7 == 0 {
			item.Evidence = []EvidenceRef{{Type: "file_path", Ref: "internal/context/retrieval.go"}}
		}
		items = append(items, item)
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetMany(context.Background(), []string{"item-03", "item-05", "item-11"})
	if err != nil {
		t.Fatal(err)
	}
	vector := fixedVector{results: []SearchResult{{Item: stored[0], Score: 0.9}, {Item: stored[1], Score: 0.7}, {Item: stored[2], Score: 0.4}}}
	for _, query := range []string{"sqlite schema", "worker retry budget", "\"sqlite schema\" internal/context/retrieval.go cache", "nothing matches"} {
		for _, limit := range []int{3, 20} {
			req := SearchRequest{Query: query, Scope: Scope{ProjectID: "p"}, Limit: limit, FilePaths: []string{"internal/context/retrieval.go"}}
			options := HybridRetrievalOptions{Vector: vector, Mode: RetrievalActive}
			plain, plainTrace, err := HybridRetrieveWithOptions(context.Background(), repo, req, options)
			if err != nil {
				t.Fatal(err)
			}
			options.Observer = &RetrievalObservation{}
			observed, observedTrace, err := HybridRetrieveWithOptions(context.Background(), repo, req, options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plain, observed) || !reflect.DeepEqual(plainTrace, observedTrace) {
				t.Fatalf("query %q limit %d: observer changed the results", query, limit)
			}
			for _, candidate := range options.Observer.Candidates {
				if candidate.ExactRank == 0 && candidate.DuplicateOf == "" && candidate.RetrievalRank != 0 {
					if got := candidate.CarriedScore + candidate.LexicalRRF + candidate.VectorRRF; math.Abs(got-candidate.FusedScore) > 1e-12 {
						t.Fatalf("query %q item %s: carried+rrf = %v, fused = %v", query, candidate.ItemID, got, candidate.FusedScore)
					}
				}
			}
		}
	}
}

// BUG-01: rrf starts from the first list's raw score, so on a realistic
// corpus the carried BM25 score dominates the reciprocal-rank term.
func TestRetrievalObserverExposesCarriedLexicalScore(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var items []ContextItem
	for i := range 300 {
		items = append(items, ContextItem{ID: fmt.Sprintf("filler-%03d", i), Kind: ContextDecision, Content: fmt.Sprintf("generic filler note number %d about coordinator workers and tasks", i), Scope: Scope{ProjectID: "p"}})
	}
	items = append(items, ContextItem{ID: "hit", Kind: ContextDecision, Content: "the sqlite schema is versioned", Scope: Scope{ProjectID: "p"}})
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	obs := &RetrievalObservation{}
	if _, _, err := HybridRetrieveWithOptions(context.Background(), repo, SearchRequest{Query: "sqlite schema", Scope: Scope{ProjectID: "p"}, Limit: 5}, HybridRetrievalOptions{Mode: RetrievalActive, UnavailableReason: SemanticFallbackProjectionMissing, Observer: obs}); err != nil {
		t.Fatal(err)
	}
	hit, ok := obs.Candidate("hit")
	if !ok || hit.LexicalRank != 1 || hit.CarriedScore <= 1 || hit.LexicalRRF != 1.0/61 {
		t.Fatalf("hit observation = %+v", hit)
	}
	if len(obs.Paths) != 3 || obs.Paths[2].Executed || obs.Paths[2].UnavailableReason != SemanticFallbackProjectionMissing {
		t.Fatalf("paths = %+v", obs.Paths)
	}
}
