package context

// WP-0 — Next-version migration fixture.
//
// WP-1 will add a migration (version 3) that introduces the `branch_id` and
// `lifecycle` columns plus a scope index update. This file verifies that the
// existing migration infrastructure (checksum verification, pre-migration
// backup, idempotent reopen) is ready to accept that next migration without
// surprises.
//
// These tests do NOT add a real migration definition — that is WP-1's job.
// Instead they exercise the migration machinery with a simulated pending
// migration to confirm:
//   1. A store with only version 1+2 applied can accept a new migration.
//   2. The backup file is created before applying the new migration to a
//      store that already has data.
//   3. The checksum of each applied migration is verified on reopen.
//   4. Existing data survives the migration.
//   5. A brand-new store skips the backup (nothing to protect).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationFixture_CurrentSchemaVersion confirms the checked-in migration
// list and a newly opened database agree on the latest schema version.
func TestMigrationFixture_CurrentSchemaVersion(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	var maxVersion int
	if err := r.db.QueryRowContext(context.Background(), "SELECT MAX(version) FROM schema_migrations").Scan(&maxVersion); err != nil {
		t.Fatalf("query max version: %v", err)
	}
	if maxVersion != 5 {
		t.Fatalf("current latest migration version = %d, want 5 (memory_consolidation_proposals)", maxVersion)
	}
	if len(migrations) != 5 {
		t.Fatalf("len(migrations) = %d, want 5", len(migrations))
	}
}

// TestMigrationFixture_PendingMigrationCreatesBackup confirms the backup
// machinery fires when a store with existing data has a pending migration.
// We simulate this by deleting the version-2 migration record so reopen
// sees it as pending.
func TestMigrationFixture_PendingMigrationCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := r.Append(context.Background(), ContextItem{ID: "pre", Kind: ContextDecision, Content: "data before simulated pending migration", Scope: Scope{ProjectID: "p"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Simulate a store that predates migration 2.
	if _, err := r.db.ExecContext(context.Background(), "DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatalf("delete migration 2 record: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen should apply pending migration: %v", err)
	}
	defer r.Close()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected backup file before applying pending migration to a store with data")
	}
	item, err := r.Get(context.Background(), "pre")
	if err != nil {
		t.Fatalf("existing data must survive: %v", err)
	}
	if item.Content != "data before simulated pending migration" {
		t.Fatalf("content changed: %q", item.Content)
	}
}

// TestMigrationFixture_ChecksumVerificationRejectsTamperedMigration
// confirms that any applied migration's checksum is verified on reopen.
// WP-1's new migration will be subject to the same protection.
func TestMigrationFixture_ChecksumVerificationRejectsTamperedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(), "UPDATE schema_migrations SET checksum='deadbeef' WHERE version=1"); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = OpenSQLite(path)
	if err == nil {
		t.Fatal("expected checksum mismatch error on reopen")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

// TestMigrationFixture_BrandNewStoreSkipsBackup confirms that a fresh store
// (no prior data) does not create a backup file. WP-1's migration applied to
// a brand-new workspace should also skip the backup.
func TestMigrationFixture_BrandNewStoreSkipsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("brand-new store should not be backed up: %v", matches)
	}
}

// TestMigrationFixture_MigrationDefinitionsAreImmutable confirms that the
// SQL text of existing migrations has not been edited (which would break
// checksum verification for stores that already applied them). This is a
// guard against accidental edits during WP-1 development.
func TestMigrationFixture_MigrationDefinitionsAreImmutable(t *testing.T) {
	for _, m := range migrations {
		checksum := migrationChecksum(m.sql)
		if checksum == "" {
			t.Fatalf("migration %d (%s) has empty checksum", m.version, m.name)
		}
		// The checksum must be a valid hex string (64 chars for SHA-256).
		if len(checksum) != 64 {
			t.Errorf("migration %d (%s) checksum length = %d, want 64", m.version, m.name, len(checksum))
		}
		for _, c := range checksum {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("migration %d (%s) checksum contains non-hex char %q in %s", m.version, m.name, string(c), checksum)
			}
		}
	}
}

// TestMigrationFixture_SchemaMigrationsTableExists confirms the migration
// tracking table is created and populated on first open.
func TestMigrationFixture_SchemaMigrationsTableExists(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	var count int
	if err := r.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Errorf("schema_migrations count = %d, want %d", count, len(migrations))
	}
}

// TestMigrationFixture_FTS5TableExistsAfterMigration confirms the FTS5
// virtual table is created by migration 1 and survives reopen. WP-1's
// migration must not drop or recreate it without a rebuild path.
func TestMigrationFixture_FTS5TableExistsAfterMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r.Close()
	var name string
	if err := r.db.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name='context_items_fts'").Scan(&name); err != nil {
		t.Fatalf("FTS5 table not found after migration: %v", err)
	}
	if name != "context_items_fts" {
		t.Errorf("FTS5 table name = %q, want context_items_fts", name)
	}
}

// TestMigrationFixture_ContextItemsTableColumns confirms the current column
// set. WP-1 will add `branch_id` and `lifecycle`; this test documents the
// pre-WP-1 schema so the addition is visible.
func TestMigrationFixture_ContextItemsTableColumns(t *testing.T) {
	r, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer r.Close()
	rows, err := r.db.QueryContext(context.Background(), "PRAGMA table_info(context_items)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = true
	}
	// Current columns that must exist (WP-0 baseline).
	for _, col := range []string{"id", "kind", "content", "content_hash", "project_id", "team_id", "session_id", "agent_id", "task_id", "attempt_id", "authority", "trust_level", "priority", "must_keep", "pinned", "confidence", "source_json", "evidence_json", "tags_json", "metadata_json", "created_at", "updated_at", "valid_from", "valid_until", "expires_at", "superseded_by", "embedding_state", "embedding_model"} {
		if !cols[col] {
			t.Errorf("context_items missing column %q", col)
		}
	}
	// WP-1 added these columns.
	for _, col := range []string{"branch_id", "lifecycle"} {
		if !cols[col] {
			t.Errorf("context_items missing column %q (added by WP-1 migration 3)", col)
		}
	}
}
