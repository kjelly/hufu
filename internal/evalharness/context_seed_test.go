package evalharness

import (
	json "encoding/json/v2"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestContextFixtureSeedPreservesExplicitZeroAndCanonicalSupersession(t *testing.T) {
	workspace := t.TempDir()
	instant := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := contextstore.WithEvaluationTime(t.Context(), instant)
	items := []ContextItemFixture{
		{ID: "old", Content: "old sentinel", SupersededBy: "replacement"},
		{ID: "replacement", Content: "new sentinel", Priority: new(0), Confidence: new(0.0), Activation: &ContextActivationFixture{Phases: []string{"execute"}}},
		{ID: "rejected", Content: "rejected sentinel", Lifecycle: "rejected"},
	}
	if err := seedCanonicalContext(ctx, workspace, "project", "team", items, false, agent.DefaultMemoryLearningPolicy()); err != nil {
		t.Fatal(err)
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	replacement, err := repo.Get(ctx, "replacement")
	if err != nil || replacement.Priority != 0 || replacement.Confidence != 0 || replacement.Metadata["activation.phases"] != "execute" || !replacement.CreatedAt.Equal(instant) {
		t.Fatalf("canonical fixture = %+v, error %v", replacement, err)
	}
	old, err := repo.Get(ctx, "old")
	if err != nil || old.SupersededBy != "replacement" {
		t.Fatalf("supersession = %+v, error %v", old, err)
	}
	rows, err := repo.SearchLexical(ctx, contextstore.SearchRequest{Query: "sentinel", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}})
	if err != nil || len(rows) != 1 || rows[0].Item.ID != "replacement" {
		t.Fatalf("eligible fixture rows = %v, error %v", rows, err)
	}
}

func TestContextFixtureBranchAliasesRequireRealForkChronology(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appendMemory := func(id, branch string) {
		t.Helper()
		payload, err := json.Marshal(map[string]string{"item_id": id})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(team.RunEvent{ID: id, BranchID: branch, Type: "worker_memory_confirmed", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	appendMemory("before", "main")
	tree := team.NewSessionTree()
	feature, err := tree.CreateBranch("feature", "before", store)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tree.CreateBranch("sibling", "before", store)
	if err != nil {
		t.Fatal(err)
	}
	appendMemory("after", "main")
	appendMemory("active", feature.ID)
	appendMemory("sibling-memory", sibling.ID)
	tree.ActiveBranch = feature.ID
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []ContextItemFixture{
		{ID: "before", Scope: "ancestor_before_fork", Branch: "main"},
		{ID: "after", Scope: "ancestor_after_fork", Branch: "main"},
		{ID: "active", Scope: "active_branch", Branch: "feature"},
		{ID: "sibling-memory", Scope: "sibling_branch", Branch: "sibling"},
	} {
		id, err := validateFixtureBranch(workspace, fixture)
		if err != nil || id != tree.GetBranch(fixture.Branch).ID {
			t.Fatalf("branch alias: %q, %v", id, err)
		}
	}
	for _, fixture := range []ContextItemFixture{
		{ID: "after", Scope: "ancestor_before_fork", Branch: "main"},
		{ID: "before", Scope: "ancestor_after_fork", Branch: "main"},
		{ID: "sibling-memory", Scope: "active_branch", Branch: "sibling"},
		{ID: "missing", Scope: "active_branch", Branch: "feature"},
	} {
		if _, err := validateFixtureBranch(workspace, fixture); err == nil {
			t.Fatalf("false lineage alias accepted: %+v", fixture)
		}
	}
	if _, err := validateFixtureBranch(t.TempDir(), ContextItemFixture{ID: "implicit", Scope: "active_branch", Branch: "main"}); err == nil {
		t.Fatal("implicit main tree was accepted as a seeded branch")
	}
}

func TestContextFixtureAliasesCannotBecomeRuntimeGrants(t *testing.T) {
	base := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session"}
	workers := map[string]string{"worker": "memory-worker"}
	private, err := fixtureScope(ContextItemFixture{ID: "private", Scope: "same_worker_private", Agent: "worker"}, base, workers)
	if err != nil || private.AgentID != "memory-worker" || private.SessionID != "" {
		t.Fatalf("private fixture scope = %+v, error %v", private, err)
	}
	if _, err := fixtureScope(ContextItemFixture{ID: "private", Scope: "same_worker_private", Agent: "unknown"}, base, workers); err == nil {
		t.Fatal("unknown worker was accepted")
	}
	if _, err := fixtureScope(ContextItemFixture{ID: "parent", Scope: "ancestor_before_fork", Agent: "worker"}, base, workers); err == nil {
		t.Fatal("ancestor scope was guessed without a seeded branch")
	}
	other, err := fixtureScope(ContextItemFixture{ID: "other", Scope: "other_worker"}, base, workers)
	if err != nil || other.AgentID == private.AgentID || other.AgentID == "" {
		t.Fatalf("other-worker fixture = %+v, error %v", other, err)
	}
}

func TestContextFixtureRejectsInvalidVocabularyAndRanges(t *testing.T) {
	for _, fixture := range []ContextItemFixture{
		{ID: "bad", Content: "sentinel", Lifecycle: "CONFIRMED"},
		{ID: "bad", Content: "sentinel", Kind: "unknown"},
		{ID: "bad", Content: "sentinel", Scope: "subtree"},
		{ID: "bad", Content: "sentinel", Confidence: new(math.NaN())},
		{ID: "bad", Content: "sentinel", Priority: new(101)},
		{ID: "bad", Content: "sentinel", Activation: &ContextActivationFixture{Triggers: []string{"step_refresh"}}},
		{ID: "bad", Content: "sentinel", SupersededBy: "missing"},
	} {
		if err := validateContextFixtures([]ContextItemFixture{fixture}); err == nil {
			t.Fatalf("invalid fixture accepted: %+v", fixture)
		}
	}
}
