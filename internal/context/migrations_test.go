package context

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateIsIdempotentAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	var rowsFirst int
	if err := r.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&rowsFirst); err != nil {
		t.Fatal(err)
	}
	if rowsFirst != len(migrations) {
		t.Fatalf("expected %d applied migrations, got %d", len(migrations), rowsFirst)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var rowsSecond int
	if err := r.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&rowsSecond); err != nil {
		t.Fatal(err)
	}
	if rowsSecond != rowsFirst {
		t.Fatalf("reopening re-ran or duplicated migrations: first=%d second=%d", rowsFirst, rowsSecond)
	}
}

func TestOpenSQLiteRejectsTamperedMigrationChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(context.Background(), "UPDATE schema_migrations SET checksum='tampered' WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenSQLite(path)
	if err == nil {
		t.Fatal("expected OpenSQLite to reject a tampered migration checksum")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum mismatch error, got: %v", err)
	}
}

func TestMigrateBacksUpExistingStoreBeforeApplyingNewMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Append(context.Background(), ContextItem{ID: "pre-migration", Kind: ContextDecision, Content: "existing data before the new migration", Scope: Scope{ProjectID: "p"}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a store that was created before migration 2 existed: it only
	// ever recorded version 1.
	if _, err := r.db.ExecContext(context.Background(), "DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopening an older store should apply the missing migration, not fail: %v", err)
	}
	defer r.Close()

	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected a backup file to be created before applying the new migration to an existing store")
	}

	var version2Count int
	if err := r.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations WHERE version=2").Scan(&version2Count); err != nil {
		t.Fatal(err)
	}
	if version2Count != 1 {
		t.Fatalf("expected migration 2 to be (re-)applied, got count=%d", version2Count)
	}
	item, err := r.Get(context.Background(), "pre-migration")
	if err != nil {
		t.Fatalf("existing data must survive the migration: %v", err)
	}
	if item.Content != "existing data before the new migration" {
		t.Fatalf("unexpected content after migration: %q", item.Content)
	}
}

func TestMigrateSkipsBackupForBrandNewStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	r, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("a brand-new store has nothing to protect and should not be backed up: %v", matches)
	}
}
