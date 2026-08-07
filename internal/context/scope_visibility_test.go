package context

// WP-1 — Scope visibility, lifecycle, and branch schema tests.
//
// These tests verify the WP-1 acceptance criteria:
//   - Agent A cannot see Agent B's private items.
//   - Coordinator shared query cannot see any agent child.
//   - Agent A can see shared ancestors + own private items.
//   - Query, FTS, and vector paths produce consistent results.
//   - Private append does not leak into stm.md/ltm.md projections.
//   - Migration preserves existing data and backfills lifecycle=confirmed.
//   - Lifecycle filter excludes candidate items from runtime recall.
//   - VisibilityExact and VisibilitySubtree behave as specified.
//   - QuerySharedProjection returns only shared-scope items.
//   - Pending write JSON is backward compatible with missing BranchID.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// --- Migration 3: schema and backfill ---

func TestWP1_MigrationAddsBranchIDAndLifecycleColumns(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	rows, err := r.db.QueryContext(ctx, "PRAGMA table_info(context_items)")
	if err != nil {
		t.Fatalf("PRAGMA: %v", err)
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = ctype
	}
	if _, ok := cols["branch_id"]; !ok {
		t.Error("missing branch_id column")
	}
	if _, ok := cols["lifecycle"]; !ok {
		t.Error("missing lifecycle column")
	}
}

func TestWP1_MigrationBackfillsExistingRowsAsConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.sqlite")
	// Create a pre-WP-1 database with only migrations 1+2 applied.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	// Apply migrations 1+2 manually.
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("exec migration %d: %v", m.version, err)
		}
	}
	// Record migrations 1+2 as applied.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL, checksum TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES (?, ?, ?, ?)", m.version, m.name, 0, migrationChecksum(m.sql)); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}
	// Insert a pre-WP-1 item directly (no branch_id/lifecycle columns exist).
	if _, err := db.Exec(`INSERT INTO context_items (id, kind, content, content_hash, project_id, team_id, authority, trust_level, priority, source_json, evidence_json, tags_json, metadata_json, created_at, updated_at, embedding_state) VALUES ('pre-wp1', 'decision', 'data before WP-1 migration', 'abc', 'p', 'team', 'agent', 'internal', 50, '{}', '[]', '[]', '{}', 1000, 1000, 'pending')`); err != nil {
		t.Fatalf("insert pre-WP-1 item: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Reopen with OpenSQLite — migration 3 should apply and backfill lifecycle.
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r.Close()
	item, err := r.Get(ctx, "pre-wp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Lifecycle != LifecycleConfirmed {
		t.Errorf("existing row lifecycle = %q, want confirmed (backfill)", item.Lifecycle)
	}
	if item.Scope.BranchID != "" {
		t.Errorf("existing row branch_id = %q, want empty (shared legacy)", item.Scope.BranchID)
	}
}

func TestWP1_MigrationPreservesExistingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.sqlite")
	// Create a pre-WP-1 database with only migrations 1+2 applied.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("exec migration %d: %v", m.version, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL, checksum TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES (?, ?, ?, ?)", m.version, m.name, 0, migrationChecksum(m.sql)); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}
	// Insert pre-WP-1 items directly.
	for _, item := range []struct{ id, content string }{
		{"keep-1", "decision to preserve"},
		{"keep-2", "progress to preserve"},
	} {
		if _, err := db.Exec(`INSERT INTO context_items (id, kind, content, content_hash, project_id, team_id, authority, trust_level, priority, source_json, evidence_json, tags_json, metadata_json, created_at, updated_at, embedding_state) VALUES (?, 'decision', ?, 'hash-?', 'p', 'team', 'agent', 'internal', 50, '{}', '[]', '[]', '{}', 1000, 1000, 'pending')`, item.id, item.content); err != nil {
			t.Fatalf("insert %s: %v", item.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r.Close()
	for _, want := range []struct{ id, content string }{
		{"keep-1", "decision to preserve"},
		{"keep-2", "progress to preserve"},
	} {
		got, err := r.Get(ctx, want.id)
		if err != nil {
			t.Fatalf("Get %s: %v", want.id, err)
		}
		if got.Content != want.content {
			t.Errorf("item %s content changed: %q", want.id, got.Content)
		}
	}
}

// --- Lifecycle filter ---

func TestWP1_LifecycleFilterExcludesCandidatesByDefault(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx,
		ContextItem{ID: "confirmed-item", Kind: ContextDecision, Content: "confirmed decision", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "candidate-item", Kind: ContextDecision, Content: "candidate guess", Scope: scope, Lifecycle: LifecycleCandidate},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "confirmed-item") {
		t.Errorf("confirmed item missing from default query: %v", ids)
	}
	if containsID(ids, "candidate-item") {
		t.Errorf("candidate item leaked into default query: %v", ids)
	}
}

func TestWP1_LifecycleFilterIncludesCandidatesWhenRequested(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx,
		ContextItem{ID: "confirmed-item", Kind: ContextDecision, Content: "confirmed decision", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "candidate-item", Kind: ContextDecision, Content: "candidate guess", Scope: scope, Lifecycle: LifecycleCandidate},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope, IncludeCandidates: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "confirmed-item") || !containsID(ids, "candidate-item") {
		t.Errorf("IncludeCandidates should return both items: %v", ids)
	}
}

func TestWP1_LifecycleFilterAppliesToSearchExact(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx,
		ContextItem{ID: "confirmed-exact", Kind: ContextObservation, Content: "searchable confirmed content", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "candidate-exact", Kind: ContextObservation, Content: "searchable candidate content", Scope: scope, Lifecycle: LifecycleCandidate},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.SearchExact(ctx, SearchRequest{Query: "searchable", Scope: scope})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "candidate-exact") {
		t.Errorf("candidate leaked into SearchExact: %v", ids)
	}
	if !containsID(ids, "confirmed-exact") {
		t.Errorf("confirmed missing from SearchExact: %v", ids)
	}
}

func TestWP1_LifecycleFilterAppliesToSearchLexical(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx,
		ContextItem{ID: "confirmed-lex", Kind: ContextObservation, Content: "lexical confirmed content", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "candidate-lex", Kind: ContextObservation, Content: "lexical candidate content", Scope: scope, Lifecycle: LifecycleCandidate},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.SearchLexical(ctx, SearchRequest{Query: "lexical", Scope: scope})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "candidate-lex") {
		t.Errorf("candidate leaked into SearchLexical: %v", ids)
	}
}

func TestWP1_LifecycleFilterAppliesToVector(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := repo.Append(ctx,
		ContextItem{ID: "confirmed-vec", Kind: ContextPattern, Content: "schema migration", Scope: scope, Lifecycle: LifecycleConfirmed},
		ContextItem{ID: "candidate-vec", Kind: ContextPattern, Content: "database upgrade", Scope: scope, Lifecycle: LifecycleCandidate},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatalf("NewVectorStore: %v", err)
	}
	if err := store.Rebuild(ctx, repo, scope); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got, err := store.SearchVector(ctx, SearchRequest{Query: "database upgrade", Scope: scope, Limit: 5})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	ids := idsOfResults(got)
	if containsID(ids, "candidate-vec") {
		t.Errorf("candidate leaked into vector search: %v", ids)
	}
}

// --- VisibilityExact ---

func TestWP1_VisibilityExactRequiresNullForEmptyFields(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	if err := r.Append(ctx,
		ContextItem{ID: "shared-item", Kind: ContextDecision, Content: "shared content", Scope: shared},
		ContextItem{ID: "agent-item", Kind: ContextObservation, Content: "agent content", Scope: agentA},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// VisibilityExact with shared scope: only shared items (all child fields NULL).
	got, err := r.Query(ctx, RepositoryQuery{
		Scope:      shared,
		Visibility: VisibilityExact,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "shared-item") {
		t.Errorf("VisibilityExact should include shared item: %v", ids)
	}
	if containsID(ids, "agent-item") {
		t.Errorf("VisibilityExact should exclude agent-scoped item: %v", ids)
	}
}

func TestWP1_VisibilityExactMatchesNonEmptyFieldsExactly(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	agentA := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	agentB := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-b"}
	if err := r.Append(ctx,
		ContextItem{ID: "a-item", Kind: ContextObservation, Content: "agent a content", Scope: agentA},
		ContextItem{ID: "b-item", Kind: ContextObservation, Content: "agent b content", Scope: agentB},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// VisibilityExact with agent-a scope: only agent-a items (no shared ancestors).
	got, err := r.Query(ctx, RepositoryQuery{
		Scope:      agentA,
		Visibility: VisibilityExact,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "a-item") {
		t.Errorf("VisibilityExact should include agent-a item: %v", ids)
	}
	if containsID(ids, "b-item") {
		t.Errorf("VisibilityExact should exclude agent-b item: %v", ids)
	}
}

// --- VisibilitySubtree (maintenance) ---

func TestWP1_VisibilitySubtreeActsAsWildcard(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	if err := r.Append(ctx,
		ContextItem{ID: "shared-item", Kind: ContextDecision, Content: "shared content", Scope: shared},
		ContextItem{ID: "agent-item", Kind: ContextObservation, Content: "agent content", Scope: agentA},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// VisibilitySubtree with shared scope (empty AgentID): wildcard sees all.
	got, err := r.Query(ctx, RepositoryQuery{
		Scope:      shared,
		Visibility: VisibilitySubtree,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "shared-item") || !containsID(ids, "agent-item") {
		t.Errorf("VisibilitySubtree should see all items (wildcard): %v", ids)
	}
}

// --- QuerySharedProjection ---

func TestWP1_QuerySharedProjectionReturnsOnlySharedItems(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	taskScoped := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a", TaskID: "task-1"}
	if err := r.Append(ctx,
		ContextItem{ID: "shared-1", Kind: ContextDecision, Content: "shared decision", Scope: shared},
		ContextItem{ID: "shared-2", Kind: ContextProgress, Content: "shared progress", Scope: shared},
		ContextItem{ID: "agent-1", Kind: ContextObservation, Content: "agent private", Scope: agentA},
		ContextItem{ID: "task-1", Kind: ContextToolResult, Content: "task scoped", Scope: taskScoped},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.QuerySharedProjection(ctx, shared)
	if err != nil {
		t.Fatalf("QuerySharedProjection: %v", err)
	}
	ids := idsOfItems(got)
	for _, want := range []string{"shared-1", "shared-2"} {
		if !containsID(ids, want) {
			t.Errorf("QuerySharedProjection missing shared item %q: %v", want, ids)
		}
	}
	for _, leak := range []string{"agent-1", "task-1"} {
		if containsID(ids, leak) {
			t.Errorf("QuerySharedProjection leaked private item %q: %v", leak, ids)
		}
	}
}

// --- Private append does not leak to projection ---

func TestWP1_PrivateAppendDoesNotLeakToProjection(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	if err := r.Append(ctx,
		ContextItem{ID: "shared-item", Kind: ContextDecision, Content: "shared visible content", Scope: shared},
		ContextItem{ID: "private-item", Kind: ContextObservation, Content: "agent-a private secret", Scope: agentA},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := r.RebuildProjection(ctx, shared); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	stm, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(stm), "shared visible content") {
		t.Errorf("projection missing shared item:\n%s", stm)
	}
	if strings.Contains(string(stm), "agent-a private secret") {
		t.Errorf("projection leaked private item:\n%s", stm)
	}
}

// --- Cross-path consistency ---

func TestWP1_AllPathsAgentASeesSharedAndOwn(t *testing.T) {
	r, _ := scopeMatrixFixture(t)
	ctx := context.Background()
	agentA := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}

	queryItems, err := r.Query(ctx, RepositoryQuery{Scope: agentA})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	exactResults, err := r.SearchExact(ctx, SearchRequest{Query: "gamma", Scope: agentA})
	if err != nil {
		t.Fatalf("SearchExact: %v", err)
	}
	lexicalResults, err := r.SearchLexical(ctx, SearchRequest{Query: "gamma", Scope: agentA})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	// All paths should see agent-a-finding.
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"Query", idsOfItems(queryItems)},
		{"SearchExact", idsOfResults(exactResults)},
		{"SearchLexical", idsOfResults(lexicalResults)},
	} {
		if !containsID(tc.ids, "agent-a-finding") {
			t.Errorf("%s path missing agent-a own item: %v", tc.name, tc.ids)
		}
		if containsID(tc.ids, "agent-b-finding") {
			t.Errorf("%s path leaked agent-b item: %v", tc.name, tc.ids)
		}
	}
}

// --- BranchID scope isolation ---

func TestWP1_BranchIDIsolation(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	mainBranch := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "agent-a"}
	featureBranch := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "feature", AgentID: "agent-a"}
	if err := r.Append(ctx,
		ContextItem{ID: "main-item", Kind: ContextObservation, Content: "main branch memory", Scope: mainBranch},
		ContextItem{ID: "feature-item", Kind: ContextObservation, Content: "feature branch memory", Scope: featureBranch},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Agent A on main branch should see own main + shared, NOT feature branch.
	got, err := r.Query(ctx, RepositoryQuery{Scope: mainBranch})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if !containsID(ids, "main-item") {
		t.Errorf("main branch query missing own item: %v", ids)
	}
	if containsID(ids, "feature-item") {
		t.Errorf("main branch query leaked feature branch item: %v", ids)
	}
}

// --- Pending write backward compat ---

func TestWP1_PendingWriteBackwardCompatMissingBranchID(t *testing.T) {
	dir := t.TempDir()
	pendingPath := filepath.Join(dir, "context-pending.jsonl")
	// Simulate an old pending write record that lacks branch_id in the scope.
	oldRecord := `{"item":{"id":"old-pending","kind":"progress","content":"old content","content_hash":"abc","scope":{"project_id":"p","team_id":"team"},"authority":"agent","trust_level":"internal","priority":50,"source":{"type":"legacy"},"embedding_state":"pending"},"cause":"old failure"}`
	if err := os.WriteFile(pendingPath, []byte(oldRecord+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Read it back — should deserialize without error.
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var pw PendingWrite
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &pw); err != nil {
		t.Fatalf("unmarshal old pending write: %v", err)
	}
	if pw.Item.Scope.ProjectID != "p" {
		t.Errorf("project_id = %q, want p", pw.Item.Scope.ProjectID)
	}
	if pw.Item.Scope.BranchID != "" {
		t.Errorf("branch_id = %q, want empty (old record)", pw.Item.Scope.BranchID)
	}
}

// --- Default lifecycle on append ---

func TestWP1_AppendDefaultsLifecycleToConfirmed(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.Append(ctx, ContextItem{ID: "default-lc", Kind: ContextDecision, Content: "no lifecycle set", Scope: Scope{ProjectID: "p"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	item, err := r.Get(ctx, "default-lc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Lifecycle != LifecycleConfirmed {
		t.Errorf("default lifecycle = %q, want confirmed", item.Lifecycle)
	}
}

// --- Rejected lifecycle excluded ---

func TestWP1_RejectedLifecycleExcludedFromDefaultQuery(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx,
		ContextItem{ID: "rejected-item", Kind: ContextDecision, Content: "rejected content", Scope: scope, Lifecycle: LifecycleRejected},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rejected item should be excluded from default query: %v", idsOfItems(got))
	}
	// IncludeCandidates should include rejected too (maintenance).
	got, err = r.Query(ctx, RepositoryQuery{Scope: scope, IncludeCandidates: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !containsID(idsOfItems(got), "rejected-item") {
		t.Errorf("rejected item should appear with IncludeCandidates: %v", idsOfItems(got))
	}
}

// --- Scope with BranchID in ancestor visibility ---

func TestWP1_AncestorVisibilityWithBranchID(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess"}
	sharedWithBranch := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main"}
	agentOnBranch := Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "agent-a"}
	if err := r.Append(ctx,
		ContextItem{ID: "shared-no-branch", Kind: ContextDecision, Content: "shared no branch", Scope: shared},
		ContextItem{ID: "shared-branch", Kind: ContextDecision, Content: "shared on main branch", Scope: sharedWithBranch},
		ContextItem{ID: "agent-branch", Kind: ContextObservation, Content: "agent on main branch", Scope: agentOnBranch},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Agent A on main branch: ancestor visibility should see shared-no-branch
	// (branch_id IS NULL ancestor), shared-branch (exact match), and own item.
	got, err := r.Query(ctx, RepositoryQuery{Scope: agentOnBranch})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	for _, want := range []string{"shared-no-branch", "shared-branch", "agent-branch"} {
		if !containsID(ids, want) {
			t.Errorf("agent on main branch missing %q: %v", want, ids)
		}
	}
}

// --- Temporal validity still works with new columns ---

func TestWP1_TemporalValidityWithNewSchema(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	if err := r.Append(ctx,
		ContextItem{ID: "active", Kind: ContextObservation, Content: "active item", Scope: scope},
		ContextItem{ID: "not-yet", Kind: ContextObservation, Content: "future item", Scope: scope, ValidFrom: &future},
		ContextItem{ID: "expired", Kind: ContextObservation, Content: "past item", Scope: scope, ValidUntil: &past},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	ids := idsOfItems(got)
	if len(ids) != 1 || !containsID(ids, "active") {
		t.Errorf("temporal filter with new schema failed: %v", ids)
	}
}
