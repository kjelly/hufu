package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteRepositoryPersistsScopeSearchAndProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{ProjectID: "project", TeamID: "team", SessionID: "session"}
	items := []ContextItem{{ID: "one", Kind: ContextDecision, Content: "Use SQLite WAL", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, Priority: PriorityHigh}, {ID: "two", Kind: ContextError, Content: "Authorization: Bearer super-secret-token", Scope: Scope{ProjectID: "project", TeamID: "other"}, Authority: AuthorityTool, TrustLevel: TrustUntrusted, Priority: PriorityNormal}}
	if err := r.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "one" {
		t.Fatalf("scope query=%+v", got)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	found, err := r.SearchLexical(ctx, SearchRequest{Query: "SQLite", Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Item.ID != "one" {
		t.Fatalf("lexical=%+v", found)
	}
	all, err := r.Get(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all.Content, "super-secret-token") {
		t.Fatalf("secret stored: %q", all.Content)
	}
	if err := r.RebuildProjection(ctx, scope); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "SQLite WAL") {
		t.Fatalf("projection=%s", b)
	}
}

func TestSQLiteRepositoryRebuildProjectionLeavesLegacyMemoryFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project", TeamID: "team"}
	if err := r.Append(ctx, ContextItem{ID: "canonical", Kind: ContextProgress, Content: "canonical context", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal}); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{
		"stm.md":      "# \u9032\u5ea6\n- legacy STM entry",
		"ltm-team.md": "# \u5c08\u6848\u6163\u4f8b\n- legacy LTM entry",
	}
	for name, content := range legacy {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.RebuildProjection(ctx, scope); err != nil {
		t.Fatal(err)
	}
	for name, want := range legacy {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("RebuildProjection overwrote legacy %s: got %q, want %q", name, got, want)
		}
	}
}

func TestSQLiteRepositorySearchExactFiltersBeforeLimit(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project", TeamID: "team"}
	items := make([]ContextItem, 0, 31)
	for n := 0; n < 30; n++ {
		items = append(items, ContextItem{ID: fmt.Sprintf("filler-%02d", n), Kind: ContextObservation, Content: fmt.Sprintf("unrelated filler %d", n), Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, Priority: PriorityHigh})
	}
	items = append(items, ContextItem{ID: "exact-target", Kind: ContextObservation, Content: "call buildTaskSTMContext before dispatch", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, Priority: PriorityLow})
	if err := r.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}

	found, err := r.SearchExact(ctx, SearchRequest{Query: "buildTaskSTMContext", Scope: scope, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Item.ID != "exact-target" {
		t.Fatalf("SearchExact failed to scan beyond its result limit: %#v", found)
	}
}

func TestSQLiteRepositoryRedactsAllRequiredSecretClassesInDBAndProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	fixtures := []struct {
		id     string
		body   string
		secret string
	}{
		{"sec-authz", "saw Authorization: Bearer sk-abc123XYZ in a log line", "sk-abc123XYZ"},
		{"sec-apikey-hdr", "request used X-API-Key: sk-99887766", "sk-99887766"},
		{"sec-apikey-eq", `config had api_key = "sk-live-1234567890abcdef"`, "sk-live-1234567890abcdef"},
		{"sec-password", "found password=SuperSecret!123 in config", "SuperSecret!123"},
		{"sec-cookie", "response set Cookie: session=zzz999yyy888", "zzz999yyy888"},
		{"sec-set-cookie", "response set Set-Cookie: session=zzz999yyy888; Path=/", "zzz999yyy888"},
		{"sec-env", "environment had DATABASE_PASSWORD=hunter2hunter2", "hunter2hunter2"},
	}
	for _, f := range fixtures {
		if err := r.Append(ctx, ContextItem{ID: f.id, Kind: ContextObservation, Content: f.body, Scope: scope, Authority: AuthorityTool, TrustLevel: TrustUntrusted}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range fixtures {
		item, err := r.Get(ctx, f.id)
		if err != nil {
			t.Fatalf("%s: %v", f.id, err)
		}
		if strings.Contains(item.Content, f.secret) {
			t.Fatalf("%s: secret leaked in DB content: %q", f.id, item.Content)
		}
	}
	if err := r.RebuildProjection(ctx, scope); err != nil {
		t.Fatal(err)
	}
	stm, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if strings.Contains(string(stm), f.secret) {
			t.Fatalf("%s: secret leaked in projection: %s", f.id, stm)
		}
	}
}

func TestSQLiteRepositorySearchLexicalRejectsOtherTeams(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	teamA := Scope{ProjectID: "project", TeamID: "team-a"}
	teamB := Scope{ProjectID: "project", TeamID: "team-b"}
	if err := r.Append(ctx,
		ContextItem{ID: "a", Kind: ContextObservation, Content: "widget factory outage", Scope: teamA, Authority: AuthorityAgent, TrustLevel: TrustInternal},
		ContextItem{ID: "b", Kind: ContextObservation, Content: "widget factory outage", Scope: teamB, Authority: AuthorityAgent, TrustLevel: TrustInternal},
	); err != nil {
		t.Fatal(err)
	}
	found, err := r.SearchLexical(ctx, SearchRequest{Query: "widget", Scope: teamA})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Item.ID != "a" {
		t.Fatalf("lexical search leaked across teams: %+v", found)
	}
}

func TestSQLiteRepositoryRebuildLexicalRestoresFTSIndex(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	if err := r.Append(ctx, ContextItem{ID: "rebuild-me", Kind: ContextPattern, Content: "rebuild lexical index", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx, "DELETE FROM context_items_fts"); err != nil {
		t.Fatal(err)
	}
	if found, err := r.SearchLexical(ctx, SearchRequest{Query: "lexical", Scope: scope}); err != nil || len(found) != 0 {
		t.Fatalf("broken FTS search=%#v err=%v", found, err)
	}
	if err := r.RebuildLexical(ctx); err != nil {
		t.Fatal(err)
	}
	found, err := r.SearchLexical(ctx, SearchRequest{Query: "lexical", Scope: scope})
	if err != nil || len(found) != 1 || found[0].Item.ID != "rebuild-me" {
		t.Fatalf("rebuilt FTS search=%#v err=%v", found, err)
	}
}

func TestSQLiteRepositorySearchExcludesItemsOutsideValidityWindow(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	if err := r.Append(ctx,
		ContextItem{ID: "active", Kind: ContextObservation, Content: "release train checklist for stable", Scope: scope},
		ContextItem{ID: "not-yet-valid", Kind: ContextObservation, Content: "release train checklist for future", Scope: scope, ValidFrom: &future},
		ContextItem{ID: "no-longer-valid", Kind: ContextObservation, Content: "release train checklist for retired", Scope: scope, ValidUntil: &past},
	); err != nil {
		t.Fatal(err)
	}
	for _, search := range []struct {
		name string
		fn   func(context.Context, SearchRequest) ([]SearchResult, error)
	}{
		{name: "exact", fn: r.SearchExact},
		{name: "lexical", fn: r.SearchLexical},
	} {
		t.Run(search.name, func(t *testing.T) {
			found, err := search.fn(ctx, SearchRequest{Query: "release train", Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 1 || found[0].Item.ID != "active" {
				t.Fatalf("search returned items outside validity window: %#v", found)
			}
		})
	}
}

func TestSQLiteRepositoryDeduplicatesAndExpires(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err = r.Append(ctx, ContextItem{Kind: ContextProgress, Content: "same", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal}); err != nil {
		t.Fatal(err)
	}
	if err = r.Append(ctx, ContextItem{Kind: ContextProgress, Content: "same", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal}); err != nil {
		t.Fatal(err)
	}
	items, err := r.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	past := time.Now().Add(-time.Hour)
	if err = r.Append(ctx, ContextItem{ID: "expired", Kind: ContextObservation, Content: "old", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	n, err := r.DeleteExpired(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
}

func TestSQLiteRepositoryRevisionAdvancesOnEveryMutation(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}

	rev0, err := r.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Append(ctx, ContextItem{ID: "keep", Kind: ContextDecision, Content: "keep me", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	rev1, err := r.Revision(ctx)
	if err != nil || rev1 <= rev0 {
		t.Fatalf("revision did not advance after Append: rev0=%d rev1=%d err=%v", rev0, rev1, err)
	}

	if err := r.Append(ctx, ContextItem{ID: "old", Kind: ContextDecision, Content: "supersede me", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	revAfterSecondAppend, err := r.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.MarkSuperseded(ctx, []string{"old"}, "keep"); err != nil {
		t.Fatal(err)
	}
	revAfterSupersede, err := r.Revision(ctx)
	if err != nil || revAfterSupersede <= revAfterSecondAppend {
		t.Fatalf("revision did not advance after MarkSuperseded: before=%d after=%d err=%v", revAfterSecondAppend, revAfterSupersede, err)
	}

	if err := r.AddEdges(ctx, ContextEdge{FromID: "keep", Relation: "derived_from", ToID: "old"}); err != nil {
		t.Fatal(err)
	}
	revAfterEdge, err := r.Revision(ctx)
	if err != nil || revAfterEdge <= revAfterSupersede {
		t.Fatalf("revision did not advance after AddEdges: before=%d after=%d err=%v", revAfterSupersede, revAfterEdge, err)
	}

	past := time.Now().Add(-time.Hour)
	if err := r.Append(ctx, ContextItem{ID: "expiring", Kind: ContextObservation, Content: "will expire", Scope: scope, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	revBeforeExpire, err := r.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.DeleteExpired(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	revAfterExpire, err := r.Revision(ctx)
	if err != nil || revAfterExpire <= revBeforeExpire {
		t.Fatalf("revision did not advance after DeleteExpired: before=%d after=%d err=%v", revBeforeExpire, revAfterExpire, err)
	}
}

func TestSQLiteRepositoryDeleteExpiredRemovesFTSRow(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	if err := r.Append(ctx, ContextItem{ID: "fts-expiring", Kind: ContextObservation, Content: "searchable but expiring", Scope: Scope{ProjectID: "p"}, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_items_fts WHERE id=?", "fts-expiring").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("expected FTS row before expiry, got count=%d", before)
	}
	if n, err := r.DeleteExpired(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	var after int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_items_fts WHERE id=?", "fts-expiring").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("expected FTS row to be removed with the expired item, still present: count=%d", after)
	}
}

func TestSQLiteRepositoryPreservesExplicitZeroConfidence(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p"}
	if err := r.Append(ctx, ContextItem{ID: "zero-conf", Kind: ContextObservation, Content: "low trust fact", Scope: scope, Confidence: 0}); err != nil {
		t.Fatal(err)
	}
	item, err := r.Get(ctx, "zero-conf")
	if err != nil {
		t.Fatal(err)
	}
	if item.Confidence != 0 {
		t.Fatalf("explicit confidence 0 was rewritten to %v", item.Confidence)
	}
}

func TestSQLiteRepositoryAppendReducerDeduplicatesByExecutionIdentity(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p", TeamID: "team", SessionID: "session"}
	base := ContextItem{Kind: ContextObservation, Content: "same finding", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, Confidence: 1, Lifecycle: LifecycleConfirmed}
	// Two distinct tasks report the same finding content. They must not
	// collapse into one item because their execution identity differs.
	itemA := base
	itemA.Metadata = map[string]string{"run_id": "run-1", "task_id": "task-a", "attempt": "1"}
	itemB := base
	itemB.Metadata = map[string]string{"run_id": "run-1", "task_id": "task-b", "attempt": "1"}
	if err := r.AppendReducer(ctx, itemA, itemB); err != nil {
		t.Fatal(err)
	}
	items, err := r.Query(ctx, RepositoryQuery{Scope: scope, Visibility: VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("AppendReducer collapsed distinct execution identities: %#v", items)
	}
	// Re-appending the same execution identity is a no-op (idempotent).
	if err := r.AppendReducer(ctx, itemA); err != nil {
		t.Fatal(err)
	}
	items, err = r.Query(ctx, RepositoryQuery{Scope: scope, Visibility: VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("AppendReducer was not idempotent for the same execution identity: %#v", items)
	}
}

func TestSQLiteRepositoryAppendReducerMergesEvidenceOnDuplicate(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "p", TeamID: "team", SessionID: "session"}
	item := ContextItem{Kind: ContextObservation, Content: "finding with evidence", Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal, Confidence: 1, Lifecycle: LifecycleConfirmed,
		Metadata: map[string]string{"run_id": "run-1", "task_id": "task-a", "attempt": "1"},
		Evidence: []EvidenceRef{{Type: "task", Ref: "task-a"}}}
	if err := r.AppendReducer(ctx, item); err != nil {
		t.Fatal(err)
	}
	// Same execution identity, new immutable evidence ref must be merged.
	item.Evidence = append(item.Evidence, EvidenceRef{Type: "artifact", Ref: "art-1"})
	if err := r.AppendReducer(ctx, item); err != nil {
		t.Fatal(err)
	}
	items, err := r.Query(ctx, RepositoryQuery{Scope: scope, Visibility: VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one merged item, got %#v", items)
	}
	types := map[string]bool{}
	for _, ev := range items[0].Evidence {
		types[ev.Type] = true
	}
	if !types["task"] || !types["artifact"] {
		t.Fatalf("duplicate append dropped immutable evidence: %#v", items[0].Evidence)
	}
}

func TestSQLiteRepositoryTenThousandItems(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	items := make([]ContextItem, 10000)
	for i := range items {
		items[i] = ContextItem{ID: fmt.Sprintf("ctx-%05d", i), Kind: ContextObservation, Content: fmt.Sprintf("observation %d", i), Scope: scope, Authority: AuthorityAgent, TrustLevel: TrustInternal}
	}
	if err := r.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}
	got, err := r.Query(ctx, RepositoryQuery{Scope: scope, Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10000 {
		t.Fatalf("query returned %d items, want 10000", len(got))
	}
}
