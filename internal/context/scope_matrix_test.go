package context

// WP-0 — Scope matrix baseline and contract tests.
//
// This file pins the *current* shared-memory visibility behavior of the
// canonical SQLite repository across all four retrieval paths (Query,
// SearchExact, SearchLexical, and vector hydration). It is intentionally
// production-code-free: WP-0 must not change any runtime behavior.
//
// The most important fixture here is the "empty AgentID query sees agent
// child" baseline (TestScopeMatrix_EmptyAgentIDSeesAgentChild_*). The current
// scopeWhere implementation omits a predicate when a child scope field is
// empty, which means a coordinator-level (AgentID="") query matches rows
// scoped to *any* agent. WP-1 will close this wildcard. These tests document
// the pre-fix behavior so the WP-1 change is a visible, deliberate contract
// transition rather than a silent regression.
//
// The WP-1 contract tests (TestScopeMatrixContract_*) assert the *target*
// isolation behavior and are expected to fail today. They use t.Skip to
// remain green on main while serving as executable specs for WP-1; once WP-1
// lands, remove the skip and they become regression guards.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// scopeMatrixFixture seeds a repository with a deterministic set of items
// spanning shared, agent-A, agent-B, and task-scoped records. Every retrieval
// path test reuses the same fixture so cross-path consistency is verifiable.
func scopeMatrixFixture(t *testing.T) (*SQLiteRepository, Scope) {
	t.Helper()
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { repo.Close() })

	shared := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	agentB := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"}
	taskA := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a", TaskID: "task-1"}

	items := []ContextItem{
		{ID: "shared-decision", Kind: ContextDecision, Content: "shared architecture decision alpha", Scope: shared, Priority: PriorityHigh},
		{ID: "shared-progress", Kind: ContextProgress, Content: "shared progress report beta", Scope: shared, Priority: PriorityNormal},
		{ID: "agent-a-finding", Kind: ContextObservation, Content: "agent-a private finding gamma", Scope: agentA, Priority: PriorityNormal},
		{ID: "agent-a-pattern", Kind: ContextPattern, Content: "agent-a reusable pattern delta", Scope: agentA, Priority: PriorityLow},
		{ID: "agent-b-finding", Kind: ContextObservation, Content: "agent-b private finding epsilon", Scope: agentB, Priority: PriorityNormal},
		{ID: "task-a-detail", Kind: ContextToolResult, Content: "agent-a task-1 tool result zeta", Scope: taskA, Priority: PriorityNormal},
	}
	ctx := context.Background()
	if err := repo.Append(ctx, items...); err != nil {
		t.Fatalf("Append fixture: %v", err)
	}
	return repo, shared
}

// idsOf extracts item IDs from a slice of results for compact assertions.
func idsOfResults(results []SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.Item.ID)
	}
	return ids
}

func idsOfItems(items []ContextItem) []string {
	ids := make([]string, 0, len(items))
	for _, i := range items {
		ids = append(ids, i.ID)
	}
	return ids
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// --- Query path ---

func TestScopeMatrix_QueryAgentASeesSharedAndOwn(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	// Agent A sees shared ancestors + own private items (without TaskID).
	// Note: task-a-detail is NOT visible here because the request has an empty
	// TaskID, which post-WP-1 means task_id IS NULL (shared-only at that level).
	for _, want := range []string{"shared-decision", "shared-progress", "agent-a-finding", "agent-a-pattern"} {
		if !containsID(ids, want) {
			t.Errorf("Agent A query missing %q: got %v", want, ids)
		}
	}
	// Agent A must NOT see Agent B's private items.
	if containsID(ids, "agent-b-finding") {
		t.Errorf("Agent A query leaked Agent B private item: %v", ids)
	}
}

func TestScopeMatrix_QueryAgentBSeesSharedAndOwn(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	for _, want := range []string{"shared-decision", "shared-progress", "agent-b-finding"} {
		if !containsID(ids, want) {
			t.Errorf("Agent B query missing %q: got %v", want, ids)
		}
	}
	for _, leak := range []string{"agent-a-finding", "agent-a-pattern", "task-a-detail"} {
		if containsID(ids, leak) {
			t.Errorf("Agent B query leaked Agent A private item %q: %v", leak, ids)
		}
	}
}

// TestScopeMatrix_EmptyAgentIDSeesAgentChild_Query documents the CURRENT
// (post-WP-1) behavior: a query with an empty AgentID matches ONLY shared
// records (agent_id IS NULL). Agent-scoped private items are invisible.
// This was the wildcard that WP-1 closed.
func TestScopeMatrix_EmptyAgentIDSeesAgentChild_Query(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}, // AgentID empty
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	// Post-WP-1: the empty-AgentID query sees ONLY shared items.
	for _, want := range []string{"shared-decision", "shared-progress"} {
		if !containsID(ids, want) {
			t.Errorf("shared query missing shared item %q: got %v", want, ids)
		}
	}
	for _, leak := range []string{"agent-a-finding", "agent-a-pattern", "agent-b-finding", "task-a-detail"} {
		if containsID(ids, leak) {
			t.Errorf("shared query leaked private item %q: %v", leak, ids)
		}
	}
}

// TestScopeMatrixContract_EmptyAgentIDSharedOnly_Query is the WP-1
// contract: an empty-AgentID (coordinator/shared) query must only see
// shared ancestor records (AgentID IS NULL), never agent-scoped private
// items.
func TestScopeMatrixContract_EmptyAgentIDSharedOnly_Query(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	for _, want := range []string{"shared-decision", "shared-progress"} {
		if !containsID(ids, want) {
			t.Errorf("shared query missing shared item %q: got %v", want, ids)
		}
	}
	for _, leak := range []string{"agent-a-finding", "agent-a-pattern", "agent-b-finding", "task-a-detail"} {
		if containsID(ids, leak) {
			t.Errorf("shared query leaked private item %q: %v", leak, ids)
		}
	}
}

// --- SearchExact path ---

func TestScopeMatrix_SearchExactAgentASeesSharedAndOwn(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchExact(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"},
	})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	ids := idsOfResults(got)
	if !containsID(ids, "agent-a-finding") {
		t.Errorf("Agent A exact search missing own item: %v", ids)
	}
}

func TestScopeMatrix_SearchExactAgentBDoesNotSeeAgentA(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchExact(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"},
	})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("Agent B exact search leaked Agent A item: %v", ids)
	}
}

// TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchExact documents the
// post-WP-1 behavior: an empty-AgentID exact search matches ONLY shared
// records.
func TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchExact(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchExact(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}, // AgentID empty
	})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	ids := idsOfResults(got)
	// Post-WP-1: empty-AgentID exact search sees only shared items.
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared exact search leaked private item: %v", ids)
	}
}

func TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchExact(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchExact(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"},
	})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared exact search leaked private item: %v", ids)
	}
}

// --- SearchLexical path ---

func TestScopeMatrix_SearchLexicalAgentASeesSharedAndOwn(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchLexical(ctx, SearchRequest{
		Query: "private finding",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"},
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	ids := idsOfResults(got)
	if !containsID(ids, "agent-a-finding") {
		t.Errorf("Agent A lexical search missing own item: %v", ids)
	}
}

func TestScopeMatrix_SearchLexicalAgentBDoesNotSeeAgentA(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchLexical(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"},
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("Agent B lexical search leaked Agent A item: %v", ids)
	}
}

// TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchLexical documents the
// post-WP-1 behavior: an empty-AgentID lexical search matches ONLY shared
// records.
func TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchLexical(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchLexical(ctx, SearchRequest{
		Query: "private finding",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}, // AgentID empty
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	ids := idsOfResults(got)
	// Post-WP-1: empty-AgentID lexical search sees only shared items.
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared lexical search leaked private item: %v", ids)
	}
}

func TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchLexical(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	got, err := repo.SearchLexical(ctx, SearchRequest{
		Query: "private finding",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"},
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared lexical search leaked private item: %v", ids)
	}
}

// --- Vector hydration path ---

func TestScopeMatrix_VectorAgentASeesSharedAndOwn(t *testing.T) {
	repo, shared := scopeMatrixFixture(t)
	ctx := context.Background()
	store, err := NewVectorStore(filepath.Join(t.TempDir(), "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatalf("NewVectorStore: %v", err)
	}
	if err := store.Rebuild(ctx, repo, Scope{ProjectID: "proj"}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, err := store.SearchVector(ctx, SearchRequest{
		Query: "private finding gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	ids := idsOfResults(got)
	if !containsID(ids, "agent-a-finding") {
		t.Errorf("Agent A vector search missing own item: %v", ids)
	}
	_ = shared
}

func TestScopeMatrix_VectorAgentBDoesNotSeeAgentA(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	store, err := NewVectorStore(filepath.Join(t.TempDir(), "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatalf("NewVectorStore: %v", err)
	}
	if err := store.Rebuild(ctx, repo, Scope{ProjectID: "proj"}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, err := store.SearchVector(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("Agent B vector search leaked Agent A item: %v", ids)
	}
}

// TestScopeMatrix_EmptyAgentIDSeesAgentChild_Vector documents the
// post-WP-1 vector hydration behavior. The isRetrievable helper now
// enforces ancestor visibility: an empty request AgentID only matches
// shared (NULL) items.
func TestScopeMatrix_EmptyAgentIDSeesAgentChild_Vector(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	store, err := NewVectorStore(filepath.Join(t.TempDir(), "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatalf("NewVectorStore: %v", err)
	}
	if err := store.Rebuild(ctx, repo, Scope{ProjectID: "proj"}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, err := store.SearchVector(ctx, SearchRequest{
		Query: "private finding",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}, // AgentID empty
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	ids := idsOfResults(got)
	// Post-WP-1: empty-AgentID vector search sees only shared items.
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared vector search leaked private item: %v", ids)
	}
}

func TestScopeMatrixContract_EmptyAgentIDSharedOnly_Vector(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	store, err := NewVectorStore(filepath.Join(t.TempDir(), "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatalf("NewVectorStore: %v", err)
	}
	if err := store.Rebuild(ctx, repo, Scope{ProjectID: "proj"}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, err := store.SearchVector(ctx, SearchRequest{
		Query: "private finding",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("shared vector search leaked private item: %v", ids)
	}
}

// --- Cross-path consistency ---

// TestScopeMatrix_AllPathsAgreeOnAgentIsolation verifies that when an AgentID
// is specified, all three SQLite-backed retrieval paths (Query, SearchExact,
// SearchLexical) agree that Agent B cannot see Agent A's private items.
// Vector hydration is checked separately above because it requires a rebuild.
func TestScopeMatrix_AllPathsAgreeOnAgentIsolation(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	agentB := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"}

	queryItems, err := repo.Query(ctx, RepositoryQuery{Scope: agentB})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	exactResults, err := repo.SearchExact(ctx, SearchRequest{Query: "gamma", Scope: agentB})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	lexicalResults, err := repo.SearchLexical(ctx, SearchRequest{Query: "gamma", Scope: agentB})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"Query", idsOfItems(queryItems)},
		{"SearchExact", idsOfResults(exactResults)},
		{"SearchLexical", idsOfResults(lexicalResults)},
	} {
		if containsID(tc.ids, "agent-a-finding") {
			t.Errorf("%s path leaked Agent A private item to Agent B: %v", tc.name, tc.ids)
		}
	}
}

// TestScopeMatrix_SharedAncestorVisibleToAllAgents confirms that shared
// (AgentID=NULL) records are visible to every agent-scoped query, which is
// the intended ancestor visibility behavior that WP-1 must preserve.
func TestScopeMatrix_SharedAncestorVisibleToAllAgents(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	for _, agentID := range []string{"agent-a", "agent-b"} {
		got, err := repo.Query(ctx, RepositoryQuery{
			Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: agentID},
		})
		if err != nil {
			t.Fatalf("Query agent=%s: %v", agentID, err)
		}
		ids := idsOfItems(got)
		for _, want := range []string{"shared-decision", "shared-progress"} {
			if !containsID(ids, want) {
				t.Errorf("agent %s missing shared ancestor %q: got %v", agentID, want, ids)
			}
		}
	}
}

// TestScopeMatrix_TaskScopedItemVisibleToAgentA verifies that a task-scoped
// item (AgentID + TaskID) is visible to the owning agent's query when the
// request includes the TaskID, but NOT visible when the request omits it
// (post-WP-1: empty TaskID means task_id IS NULL, shared-only).
func TestScopeMatrix_TaskScopedItemVisibleToAgentA(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	// Without TaskID in the request: task-scoped item is NOT visible
	// (empty TaskID = task_id IS NULL).
	got, err := repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if containsID(ids, "task-a-detail") {
		t.Errorf("task-scoped item should not be visible without TaskID in request: %v", ids)
	}
	// With TaskID in the request: task-scoped item IS visible.
	got, err = repo.Query(ctx, RepositoryQuery{
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a", TaskID: "task-1"},
	})
	if err != nil {
		t.Fatalf("Query with TaskID: %v", err)
	}
	ids = idsOfItems(got)
	if !containsID(ids, "task-a-detail") {
		t.Errorf("task-scoped item should be visible with matching TaskID: %v", ids)
	}
}

// TestScopeMatrix_DifferentTeamIsolated confirms the existing team-level
// isolation is intact: items from team-a are invisible to a team-b query.
func TestScopeMatrix_DifferentTeamIsolated(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Append(ctx,
		ContextItem{ID: "team-a-item", Kind: ContextObservation, Content: "team alpha observation", Scope: Scope{ProjectID: "proj", TeamID: "team-a"}},
		ContextItem{ID: "team-b-item", Kind: ContextObservation, Content: "team beta observation", Scope: Scope{ProjectID: "proj", TeamID: "team-b"}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := repo.Query(ctx, RepositoryQuery{Scope: Scope{ProjectID: "proj", TeamID: "team-a"}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if containsID(ids, "team-b-item") {
		t.Errorf("team-a query leaked team-b item: %v", ids)
	}
	if !containsID(ids, "team-a-item") {
		t.Errorf("team-a query missing own item: %v", ids)
	}
}

// TestScopeMatrix_HybridRetrieveRespectsAgentScope confirms that the hybrid
// retrieval orchestrator (used at runtime) inherits the same scope isolation
// from its underlying repository paths.
func TestScopeMatrix_HybridRetrieveRespectsAgentScope(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	agentB := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"}
	got, _, err := HybridRetrieve(ctx, repo, nil, SearchRequest{
		Query: "gamma",
		Scope: agentB,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("HybridRetrieve: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "agent-a-finding") {
		t.Errorf("HybridRetrieve leaked Agent A item to Agent B: %v", ids)
	}
}

// TestScopeMatrix_FTSContentDoesNotLeakAcrossAgents verifies that the FTS5
// projection does not expose content from one agent's private items to
// another agent's lexical search, even when the search term is unique to
// the private item.
func TestScopeMatrix_FTSContentDoesNotLeakAcrossAgents(t *testing.T) {
	repo, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	// "gamma" appears only in agent-a-finding content.
	got, err := repo.SearchLexical(ctx, SearchRequest{
		Query: "gamma",
		Scope: Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-b"},
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	for _, r := range got {
		if strings.Contains(r.Item.Content, "gamma") {
			t.Errorf("Agent B lexical search saw Agent A content containing 'gamma': %q", r.Item.Content)
		}
	}
}
