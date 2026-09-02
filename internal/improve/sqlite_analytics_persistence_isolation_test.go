package improve

// Persistence isolation, end to end (spec.md §16.2). The tests in
// sqlite_analytics_test.go prove the analytics session itself never creates
// a file. That leaves a gap spec.md §16.2 explicitly calls out: a workspace
// can already have a real, migrated canonical context.sqlite with data in
// it, and nothing so far proves a full AnalyzeRecent run leaves that
// database's schema and rows completely untouched. These tests build a real
// canonical store via contextstore.OpenSQLite (the same entry point
// production code uses), snapshot its schema and row content, run a full
// AnalyzeRecent, and assert the snapshot is byte-for-byte identical
// afterward — including schema_migrations, per spec.md §16.2's explicit
// requirement.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"

	_ "modernc.org/sqlite"
)

// contextDBSnapshot is a logical (not byte-level) snapshot of the canonical
// store: schema objects, applied migrations, and every context_items row.
// Comparing logical content rather than raw file bytes avoids false
// failures from WAL/page-layout churn that carries no semantic change.
type contextDBSnapshot struct {
	Schema     []string
	Migrations []string
	Items      []string
}

func snapshotContextDB(t *testing.T, path string) contextDBSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open context.sqlite for snapshot: %v", err)
	}
	defer func() { _ = db.Close() }()

	snap := contextDBSnapshot{}

	schemaRows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	for schemaRows.Next() {
		var typ, name, sqlText string
		if err := schemaRows.Scan(&typ, &name, &sqlText); err != nil {
			_ = schemaRows.Close()
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		snap.Schema = append(snap.Schema, typ+":"+name+":"+sqlText)
	}
	if err := schemaRows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	_ = schemaRows.Close()

	migrationRows, err := db.Query(`SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	for migrationRows.Next() {
		var version int
		var name, checksum string
		if err := migrationRows.Scan(&version, &name, &checksum); err != nil {
			_ = migrationRows.Close()
			t.Fatalf("scan schema_migrations row: %v", err)
		}
		snap.Migrations = append(snap.Migrations, strings.Join([]string{
			strconv.Itoa(version), name, checksum,
		}, ":"))
	}
	if err := migrationRows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	_ = migrationRows.Close()

	itemRows, err := db.Query(`SELECT id, content, content_hash, project_id, lifecycle, created_at, updated_at FROM context_items ORDER BY id`)
	if err != nil {
		t.Fatalf("query context_items: %v", err)
	}
	for itemRows.Next() {
		var id, content, contentHash, projectID, lifecycle string
		var createdAt, updatedAt int64
		if err := itemRows.Scan(&id, &content, &contentHash, &projectID, &lifecycle, &createdAt, &updatedAt); err != nil {
			_ = itemRows.Close()
			t.Fatalf("scan context_items row: %v", err)
		}
		row, _ := json.Marshal([]any{id, content, contentHash, projectID, lifecycle, createdAt, updatedAt})
		snap.Items = append(snap.Items, string(row))
	}
	if err := itemRows.Err(); err != nil {
		t.Fatalf("iterate context_items: %v", err)
	}
	_ = itemRows.Close()

	sort.Strings(snap.Schema)
	sort.Strings(snap.Migrations)
	sort.Strings(snap.Items)
	return snap
}

func TestAnalyzeRecentDoesNotModifyRealCanonicalContextSQLite(t *testing.T) {
	workspace := t.TempDir()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(filepath.Join(workspace, "logs", "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Agent: "developer", Status: "done", Usage: team.ExecutionUsage{TotalTokens: 10}},
	})

	// A real, migrated canonical store with actual rows in it — not just an
	// empty file — so a mutation would show up in the snapshot diff.
	contextPath := filepath.Join(workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLite(contextPath)
	if err != nil {
		t.Fatalf("open canonical context.sqlite: %v", err)
	}
	if err := repo.Append(context.Background(), contextstore.ContextItem{
		Content: "canonical memory item that must survive AnalyzeRecent untouched",
		Scope:   contextstore.Scope{ProjectID: "isolation-test"},
	}); err != nil {
		repo.Close()
		t.Fatalf("seed canonical context item: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close canonical repository: %v", err)
	}

	before := snapshotContextDB(t, contextPath)
	if len(before.Migrations) == 0 {
		t.Fatal("expected schema_migrations to have at least one applied migration before the test proceeds")
	}
	if len(before.Items) == 0 {
		t.Fatal("expected context_items to have the seeded row before the test proceeds")
	}

	if _, err := AnalyzeRecent(workspace, "dev", teamDir, 1); err != nil {
		t.Fatalf("AnalyzeRecent: %v", err)
	}

	after := snapshotContextDB(t, contextPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("canonical context.sqlite changed after AnalyzeRecent:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// The TEMP analytics tables must never leak into the canonical store's
	// own schema, even under a different name collision scenario.
	for _, name := range []string{"execution_events", "execution_event_skills", "audit_events", "memory_events"} {
		for _, entry := range after.Schema {
			if strings.Contains(entry, ":"+name+":") {
				t.Fatalf("canonical context.sqlite unexpectedly gained an analytics table/view: %s", entry)
			}
		}
	}
}
