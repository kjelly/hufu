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
	if err := r.RebuildProjection(ctx, Scope{ProjectID: "project"}); err != nil {
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
