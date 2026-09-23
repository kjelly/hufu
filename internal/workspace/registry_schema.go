package workspace

import (
	"context"
	"database/sql"
	"time"

	"github.com/kjelly/hufu/internal/sqlmigrate"
)

var registryMigrations = []sqlmigrate.Migration{
	{
		Version: 1,
		Name:    "initial_workspace_registry",
		SQL: `CREATE TABLE projects (
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

func migrateRegistry(ctx context.Context, db *sql.DB, now func() time.Time) error {
	return sqlmigrate.Apply(ctx, db, "registry", registryMigrations, now)
}

func verifyRegistryMigrations(ctx context.Context, db *sql.DB) error {
	return sqlmigrate.Verify(ctx, db, "registry", registryMigrations)
}
