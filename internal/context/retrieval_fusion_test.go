package context

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

func fusionTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var items []ContextItem
	for i := range 300 {
		items = append(items, ContextItem{ID: fmt.Sprintf("filler-%03d", i), Kind: ContextDecision, Content: fmt.Sprintf("generic filler note %d about coordinator workers", i), Scope: Scope{ProjectID: "p"}})
	}
	for i, content := range []string{"sqlite schema version table", "back up the sqlite schema before migrating", "sqlite schema checksum mismatch aborts the open", "sqlite schema readers check the version first", "the sqlite schema is append only"} {
		items = append(items, ContextItem{ID: fmt.Sprintf("hit-%d", i), Kind: ContextDecision, Content: content, Scope: Scope{ProjectID: "p"}})
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	return repo
}

func fusionRetrieve(t *testing.T, repo *SQLiteRepository, fusion FusionMode, vector VectorSearcher) ([]SearchResult, *RetrievalObservation) {
	t.Helper()
	obs := &RetrievalObservation{}
	options := HybridRetrievalOptions{Mode: RetrievalActive, Vector: vector, Fusion: fusion, Observer: obs}
	if vector == nil {
		options.UnavailableReason = SemanticFallbackProjectionMissing
	}
	results, _, err := HybridRetrieveWithOptions(context.Background(), repo, SearchRequest{Query: "sqlite schema", Scope: Scope{ProjectID: "p"}, Limit: 10}, options)
	if err != nil {
		t.Fatal(err)
	}
	return results, obs
}

// Lexical-only, both fusions order candidates by lexical rank before MMR.
// Legacy scores are raw BM25 (several units), so MMR's diversity penalty
// (at most 0.25) only reorders near-equal matches; normalized scores are in
// (0,1], where the penalty has the weight its lambda intends.
func TestNormalizedFusionKeepsFusedOrderAndUnitScale(t *testing.T) {
	repo := fusionTestRepo(t)
	legacy, legacyObs := fusionRetrieve(t, repo, "", nil)
	normalized, obs := fusionRetrieve(t, repo, FusionRRFNormalized, nil)
	if len(legacy) != len(normalized) || len(normalized) != 5 {
		t.Fatalf("result counts legacy=%d normalized=%d", len(legacy), len(normalized))
	}
	for _, result := range legacy {
		c, _ := legacyObs.Candidate(result.Item.ID)
		if result.Score <= 1 || c.PreMMRRank != c.LexicalRank {
			t.Fatalf("legacy %s: score %v pre_mmr_rank %d lexical_rank %d; the corpus should show BUG-01", result.Item.ID, result.Score, c.PreMMRRank, c.LexicalRank)
		}
	}
	for _, result := range normalized {
		c, _ := obs.Candidate(result.Item.ID)
		if c.PreMMRRank != c.LexicalRank || c.CarriedScore != 0 || math.Abs(c.LexicalRRF-c.FusedScore) > 1e-12 {
			t.Fatalf("normalized %s observation %+v", result.Item.ID, c)
		}
		if c.FusedScore <= 0 || c.FusedScore > 1 {
			t.Fatalf("normalized fused score %v outside (0,1]", c.FusedScore)
		}
	}
	if normalized[0].Item.ID != legacy[0].Item.ID || normalized[0].Score != 1 {
		t.Fatalf("normalized top = %s (%v), legacy top = %s", normalized[0].Item.ID, normalized[0].Score, legacy[0].Item.ID)
	}
}

func TestNormalizedFusionScalesByNonEmptyLists(t *testing.T) {
	repo := fusionTestRepo(t)
	both, err := repo.GetMany(context.Background(), []string{"hit-2", "filler-007"})
	if err != nil {
		t.Fatal(err)
	}
	vector := fixedVector{results: []SearchResult{{Item: both[0], Score: 0.9}, {Item: both[1], Score: 0.8}}}
	results, obs := fusionRetrieve(t, repo, FusionRRFNormalized, vector)
	scores := map[string]float64{}
	for _, result := range results {
		scores[result.Item.ID] = result.Score
	}
	lexicalTop := results[0].Item.ID
	lexical, _ := obs.Candidate("hit-2")
	if lexical.LexicalRank == 0 || lexical.VectorRank != 1 {
		t.Fatalf("hit-2 observation %+v", lexical)
	}
	want := (61/float64(60+lexical.LexicalRank) + 1) / 2
	if math.Abs(scores["hit-2"]-want) > 1e-12 {
		t.Fatalf("hit-2 score = %v, want %v (top %s)", scores["hit-2"], want, lexicalTop)
	}
	vectorOnly, _ := obs.Candidate("filler-007")
	if math.Abs(vectorOnly.FusedScore-61/62.0/2) > 1e-12 {
		t.Fatalf("vector-only fused = %v", vectorOnly.FusedScore)
	}
}

func TestHybridRetrieveRejectsUnknownFusion(t *testing.T) {
	repo := fusionTestRepo(t)
	if _, _, err := HybridRetrieveWithOptions(context.Background(), repo, SearchRequest{Query: "sqlite", Scope: Scope{ProjectID: "p"}}, HybridRetrievalOptions{Mode: RetrievalOff, Fusion: "rrf"}); err == nil {
		t.Fatal("unknown fusion accepted")
	}
}

func TestScoreNormalizedFusionKeepsMatchStrength(t *testing.T) {
	repo := fusionTestRepo(t)
	for _, weight := range []float64{0, 0.5, 1} {
		t.Run(fmt.Sprintf("weight=%v", weight), func(t *testing.T) {
			obs := &RetrievalObservation{}
			results, _, err := HybridRetrieveWithOptions(context.Background(), repo, SearchRequest{Query: "sqlite schema", Scope: Scope{ProjectID: "p"}, Limit: 10}, HybridRetrievalOptions{
				Mode: RetrievalActive, UnavailableReason: SemanticFallbackProjectionMissing, Fusion: FusionScoreNormalized, FusionCarriedWeight: weight, Observer: obs,
			})
			if err != nil {
				t.Fatal(err)
			}
			effective := weight
			if effective == 0 {
				effective = DefaultScoreFusionCarriedWeight
			}
			maxLexical := 0.0
			for _, c := range obs.Candidates {
				maxLexical = max(maxLexical, c.LexicalScore)
			}
			for _, result := range results {
				c, _ := obs.Candidate(result.Item.ID)
				if want := effective * c.LexicalScore / maxLexical; math.Abs(c.CarriedScore-want) > 1e-12 {
					t.Fatalf("%s carried = %v, want %v", c.ItemID, c.CarriedScore, want)
				}
				if math.Abs(c.CarriedScore+c.LexicalRRF-c.FusedScore) > 1e-12 || c.FusedScore <= 0 || c.FusedScore > 1+1e-12 {
					t.Fatalf("%s observation %+v", c.ItemID, c)
				}
			}
			// Match strength orders the fused list: a strictly stronger
			// lexical match is never ranked below a weaker one before MMR.
			for _, a := range obs.Candidates {
				for _, b := range obs.Candidates {
					if a.LexicalScore > b.LexicalScore && a.PreMMRRank > b.PreMMRRank {
						t.Fatalf("%s (%v) ranked below %s (%v) before MMR", a.ItemID, a.LexicalScore, b.ItemID, b.LexicalScore)
					}
				}
			}
			if math.Abs(results[0].Score-1) > 1e-12 {
				t.Fatalf("top fused score = %v, want 1", results[0].Score)
			}
		})
	}
	if _, _, err := HybridRetrieveWithOptions(context.Background(), repo, SearchRequest{Query: "sqlite", Scope: Scope{ProjectID: "p"}}, HybridRetrievalOptions{Mode: RetrievalOff, Fusion: FusionScoreNormalized, FusionCarriedWeight: 1.5}); err == nil {
		t.Fatal("carried weight above 1 accepted")
	}
}
