package team

import (
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestExplainHistoricalQueryUsesFrozenManifestWithoutReranking(t *testing.T) {
	c := rankingLockFixture(t, "observe")
	repo := c.contextRepo.(*contextstore.SQLiteRepository)
	items, err := repo.QuerySharedPersistentProjection(t.Context(), c.contextScope())
	if err != nil || len(items) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	item := items[0]
	plan := BuildContextQueryPlan(ContextRequest{Goal: "inspect", Constraints: "migration"}, ContextQueryHints{})
	selection, err := queryLegacyRollback(plan, []ContextQuerySource{{Kind: QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: c.contextScope(), Limit: 20}}}, QueryFallbackNoMatches)
	if err != nil {
		t.Fatal(err)
	}
	query := selection.Attribution
	manifest := MemoryInjectionManifest{RetrievalID: "historical-active", PolicyVersion: "old-policy", QueryHash: hashQueryAttribution(query), Query: &query, Items: []MemoryInjectionItem{{ContextItemID: item.ID, Rank: 2, FinalScore: .4, ScoreParts: MemoryScoreParts{BaseRelevance: .3}}}}
	session := NewSession()
	session.Tasks = []*TodoItem{{ID: "task", MemoryManifests: []MemoryInjectionManifest{manifest}}}
	if err := SaveSession(c.session.Workspace, session); err != nil {
		t.Fatal(err)
	}
	in := MemoryExplainInput{Workspace: c.session.Workspace, ProjectID: "project", TeamID: "team", ItemID: item.ID, RetrievalID: manifest.RetrievalID}
	got, err := ExplainPersistentMemory(t.Context(), repo, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recomputed || got.QueryStatus != "query_plan_required" || got.QueryPolicy == nil || got.RetrievalID != manifest.RetrievalID || got.FinalScore != .4 || got.ScoreParts.BaseRelevance != .3 || len(got.Retrieval.Paths) != 0 {
		t.Fatalf("historical selection was recomputed: %+v", got)
	}
	in.Query = "migration"
	if _, err := ExplainPersistentMemory(t.Context(), repo, in); err == nil {
		t.Fatal("single query accepted as historical plan")
	}
	in.Query = ""
	in.TeamID = "other"
	if _, err := ExplainPersistentMemory(t.Context(), repo, in); err == nil {
		t.Fatal("historical manifest bypasses item scope")
	}
}
