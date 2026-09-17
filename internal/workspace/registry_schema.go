package workspace

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type registryMigration struct {
	version int
	name    string
	sql     string
}

var registryMigrations = []registryMigration{
	{
		version: 1,
		name:    "initial_workspace_registry",
		sql: `CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL,
    alias TEXT UNIQUE,
    subject_root TEXT NOT NULL UNIQUE,
    state_dir TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('active')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER
);
CREATE TABLE workspaces (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_name TEXT NOT NULL,
    context_scope_id TEXT NOT NULL,
    control_root TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('creating','active','deleting','restoring')),
    operation_id TEXT,
    pending_path TEXT,
    requires_fresh_session INTEGER NOT NULL DEFAULT 0 CHECK (requires_fresh_session IN (0,1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE(project_id, team_name)
);
CREATE TABLE trash_workspaces (
    trash_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_name TEXT NOT NULL,
    context_scope_id TEXT NOT NULL,
    original_control_root TEXT NOT NULL,
    trash_path TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('trashed','restoring','purging')),
    operation_id TEXT,
    requires_fresh_session INTEGER NOT NULL DEFAULT 0 CHECK (requires_fresh_session IN (0,1)),
    deleted_at INTEGER NOT NULL,
    purge_after INTEGER
);
CREATE TABLE registry_operations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('create','migrate','delete','restore','purge','repair')),
    project_id TEXT,
    workspace_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('started','completed','failed')),
    detail_code TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX idx_projects_slug ON projects(slug);
CREATE INDEX idx_workspaces_project ON workspaces(project_id, team_name);
CREATE INDEX idx_trash_purge_after ON trash_workspaces(state, purge_after);
CREATE INDEX idx_operations_state ON registry_operations(state, started_at);`,
	},
}

func registryMigrationChecksum(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])
}

func migrateRegistry(ctx context.Context, db *sql.DB, now func() time.Time) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY,
name TEXT NOT NULL,
applied_at INTEGER NOT NULL,
checksum TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("create migration registry: %w", err)
	}

	applied := make(map[int]string)
	rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read registry migrations: %w", err)
	}
	for rows.Next() {
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			return fmt.Errorf("scan registry migration: %w", err)
		}
		migration, ok := registryMigrationByVersion(version)
		if !ok || migration.name != name {
			return fmt.Errorf("unknown or renamed registry migration version %d (%s)", version, name)
		}
		applied[version] = checksum
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read registry migrations: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close registry migrations query: %w", err)
	}

	for _, migration := range registryMigrations {
		checksum := registryMigrationChecksum(migration.sql)
		if existing, ok := applied[migration.version]; ok {
			if existing != checksum {
				return fmt.Errorf("registry migration checksum mismatch for version %d (%s)", migration.version, migration.name)
			}
			continue
		}
		if err = applyRegistryMigration(ctx, db, migration, checksum, now); err != nil {
			if verifyErr := verifyRegistryMigrations(ctx, db); verifyErr == nil {
				continue
			}
			return err
		}
	}
	return verifyRegistryMigrations(ctx, db)
}

func registryMigrationByVersion(version int) (registryMigration, bool) {
	for _, migration := range registryMigrations {
		if migration.version == version {
			return migration, true
		}
	}
	return registryMigration{}, false
}

func verifyRegistryMigrations(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("verify registry migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	known := make(map[int]registryMigration, len(registryMigrations))
	for _, migration := range registryMigrations {
		known[migration.version] = migration
	}
	seen := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			return fmt.Errorf("verify registry migration row: %w", err)
		}
		migration, ok := known[version]
		if !ok || migration.name != name || registryMigrationChecksum(migration.sql) != checksum {
			return fmt.Errorf("registry migration checksum mismatch for version %d (%s)", version, name)
		}
		seen++
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("verify registry migrations: %w", err)
	}
	if seen != len(registryMigrations) {
		return fmt.Errorf("registry schema is incomplete: found %d of %d migrations", seen, len(registryMigrations))
	}
	return nil
}

func applyRegistryMigration(ctx context.Context, db *sql.DB, migration registryMigration, checksum string, now func() time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin registry migration %d: %w", migration.version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, migration.sql); err != nil {
		return fmt.Errorf("apply registry migration %d (%s): %w", migration.version, migration.name, err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at,checksum) VALUES(?,?,?,?)", migration.version, migration.name, now().UnixMilli(), checksum); err != nil {
		return fmt.Errorf("record registry migration %d: %w", migration.version, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit registry migration %d: %w", migration.version, err)
	}
	return nil
}
