package context

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestContextDeliveryChineseSyntaxAndFakeEmbeddingContract(t *testing.T) {
	dir := t.TempDir()
	ctx := WithEvaluationTime(t.Context(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(ctx,
		ContextItem{ID: "relevant", Content: "請使用資料表遷移", Kind: ContextPattern, Lifecycle: LifecycleConfirmed, Scope: scope},
		ContextItem{ID: "noise", Content: "更新使用者畫面", Kind: ContextPattern, Lifecycle: LifecycleConfirmed, Scope: scope},
	); err != nil {
		t.Fatal(err)
	}
	search := SearchRequest{Query: "修正資料庫結構", Scope: scope, Limit: 1}
	lexical, err := repo.SearchLexical(ctx, search)
	if err != nil || len(lexical) != 0 {
		t.Fatalf("Chinese lexical syntax/known limitation: %v, %v", lexical, err)
	}
	embedder := newKeywordEmbedder(func(text string) []float32 {
		switch text {
		case "請使用資料表遷移", "修正資料庫結構":
			return []float32{1, 0}
		case "更新使用者畫面":
			return []float32{0, 1}
		}
		return nil
	})
	vector := newRefreshedSemanticIndex(t, repo, "project", embedder)
	if documents, _ := embedder.calls(); documents != 2 {
		t.Fatalf("seed embedding calls %d, want 2", documents)
	}
	var work QueryWork
	ctx = WithQueryWorkObserver(ctx, func(_ context.Context, observed QueryWork) { work = observed })
	results, _, err := HybridRetrieve(ctx, repo, vector, search)
	if err != nil || len(results) != 1 || results[0].Item.ID != "relevant" {
		t.Fatalf("fake semantic contract: %v, %v", results, err)
	}
	if documents, queries := embedder.calls(); documents != 2 || queries != 1 || work.VectorCalls != 1 || work.LexicalCalls != 1 {
		t.Fatalf("embedding/backend accounting: documents=%d queries=%d query=%+v", documents, queries, work)
	}
}
