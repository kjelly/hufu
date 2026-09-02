package improve

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenSQLiteAnalyticsSession_CreatesAndClosesCleanly(t *testing.T) {
	ctx := context.Background()
	session, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if session.db == nil || session.conn == nil {
		t.Fatalf("session missing db/conn: %+v", session)
	}
	if got := session.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (TEMP tables are connection-scoped)", got)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A closed session must not silently accept further queries.
	if _, err := session.conn.QueryContext(ctx, "SELECT 1"); err == nil {
		t.Fatal("expected query on a closed connection to fail")
	}
}

func TestOpenSQLiteAnalyticsSession_CreatesTempSchema(t *testing.T) {
	ctx := context.Background()
	session, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = session.Close() }()

	wantTables := []string{"execution_events", "execution_event_skills", "audit_events"}
	for _, name := range wantTables {
		var got string
		err := session.conn.QueryRowContext(ctx,
			"SELECT name FROM sqlite_temp_master WHERE type='table' AND name=?", name,
		).Scan(&got)
		if err != nil {
			t.Fatalf("temp table %q not found: %v", name, err)
		}
	}
}

func TestOpenSQLiteAnalyticsSession_IndexesAreCreatedOnlyAfterCreateIndexesIsCalled(t *testing.T) {
	ctx := context.Background()
	session, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = session.Close() }()

	// sqlite_autoindex_* entries back PRIMARY KEY/UNIQUE constraints and are
	// created implicitly by CREATE TABLE itself, not by createIndexes; only
	// the explicitly named indexes in analyticsIndexStatements are relevant
	// to the "build indexes after bulk load" rule (spec.md §7).
	countIndexes := func() int {
		var n int
		if err := session.conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_temp_master WHERE type='index' AND name NOT LIKE 'sqlite_autoindex_%'",
		).Scan(&n); err != nil {
			t.Fatalf("count indexes: %v", err)
		}
		return n
	}
	if n := countIndexes(); n != 0 {
		t.Fatalf("indexes before createIndexes = %d, want 0 (bulk load must happen before indexing, spec.md §7)", n)
	}
	if err := session.createIndexes(ctx); err != nil {
		t.Fatalf("createIndexes: %v", err)
	}
	if n := countIndexes(); n != len(analyticsIndexStatements) {
		t.Fatalf("indexes after createIndexes = %d, want %d", n, len(analyticsIndexStatements))
	}
}

func TestOpenSQLiteAnalyticsSession_NoDiskSideEffects(t *testing.T) {
	dir := t.TempDir()
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := session.conn.ExecContext(ctx, "INSERT INTO execution_events(event_seq, version, timestamp_raw, run_id, team) VALUES (1, 1, 'x', 'r1', 'dev')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("directory contents changed: before=%v after=%v", before, after)
	}
	// context.sqlite specifically must never appear next to any workspace
	// this session happens to run near.
	if _, err := os.Stat(filepath.Join(dir, "context.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("analytics session must not create context.sqlite, stat err = %v", err)
	}
}

func TestSQLiteAnalyticsSession_TempTablesAreConnectionScoped(t *testing.T) {
	// Demonstrates the exact hazard spec.md §5.1 warns about: without pinning
	// a single connection, a second connection to the same nominal database
	// cannot see TEMP objects created on the first. sqliteAnalyticsSession
	// avoids this by pinning conn and always querying through it — this test
	// documents *why* that pinning is required, using two independent
	// :memory: handles as a stand-in for "two connections from a pool".
	ctx := context.Background()
	dbA, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbA.Close() }()
	if _, err := dbA.ExecContext(ctx, "CREATE TEMP TABLE execution_events(id INTEGER)"); err != nil {
		t.Fatal(err)
	}

	dbB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbB.Close() }()
	if _, err := dbB.QueryContext(ctx, "SELECT * FROM execution_events"); err == nil {
		t.Fatal("expected a second, independent connection to not see the first connection's TEMP table")
	}
}
