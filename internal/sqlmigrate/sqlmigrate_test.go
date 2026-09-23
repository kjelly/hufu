package sqlmigrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var baseMigrations = []Migration{
	{Version: 1, Name: "create_a", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	{Version: 2, Name: "create_b", SQL: "CREATE TABLE b (id INTEGER PRIMARY KEY);"},
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fixedNow() time.Time { return time.Unix(1700000000, 0) }

func TestValidate(t *testing.T) {
	tests := []struct {
		name       string
		migrations []Migration
		wantErr    bool
	}{
		{name: "empty list", migrations: nil},
		{name: "increasing", migrations: baseMigrations},
		{name: "duplicate version", migrations: []Migration{{1, "a", "SELECT 1"}, {1, "b", "SELECT 1"}}, wantErr: true},
		{name: "decreasing version", migrations: []Migration{{2, "a", "SELECT 1"}, {1, "b", "SELECT 1"}}, wantErr: true},
		{name: "zero version", migrations: []Migration{{0, "a", "SELECT 1"}}, wantErr: true},
		{name: "missing name", migrations: []Migration{{1, "", "SELECT 1"}}, wantErr: true},
		{name: "missing sql", migrations: []Migration{{1, "a", ""}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.migrations); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyAndReopen(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		reopen  []Migration
		wantErr string
	}{
		{name: "same migrations are idempotent", reopen: baseMigrations},
		{name: "new migration is appended", reopen: append(append([]Migration(nil), baseMigrations...), Migration{3, "create_c", "CREATE TABLE c (id INTEGER);"})},
		{name: "edited migration fails closed", reopen: []Migration{baseMigrations[0], {2, "create_b", "CREATE TABLE b (id TEXT);"}}, wantErr: "test migration checksum mismatch for version 2 (create_b)"},
		{name: "renamed migration fails closed", reopen: []Migration{baseMigrations[0], {2, "create_bee", baseMigrations[1].SQL}}, wantErr: "unknown or renamed test migration version 2 (create_b)"},
		{name: "dropped migration fails closed", reopen: baseMigrations[:1], wantErr: "unknown or renamed test migration version 2 (create_b)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			if err := Apply(ctx, db, "test", baseMigrations, fixedNow); err != nil {
				t.Fatalf("initial Apply: %v", err)
			}
			err := Apply(ctx, db, "test", tt.reopen, fixedNow)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("reopen Apply: %v", err)
				}
				if verifyErr := Verify(ctx, db, "test", tt.reopen); verifyErr != nil {
					t.Fatalf("Verify after reopen: %v", verifyErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("reopen Apply err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyReportsIncompleteSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := Apply(ctx, db, "test", baseMigrations[:1], fixedNow); err != nil {
		t.Fatal(err)
	}
	err := Verify(ctx, db, "test", baseMigrations)
	if err == nil || !strings.Contains(err.Error(), "test schema is incomplete: found 1 of 2 migrations") {
		t.Fatalf("Verify err = %v", err)
	}
}

func TestApplyRollsBackFailedMigration(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	broken := []Migration{baseMigrations[0], {2, "broken", "CREATE TABLE b (id INTEGER); CREATE TABLE a (id INTEGER);"}}
	if err := Apply(ctx, db, "test", broken, fixedNow); err == nil {
		t.Fatal("Apply of a failing migration succeeded")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='b'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left table b behind")
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version=2").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration was recorded")
	}
}
