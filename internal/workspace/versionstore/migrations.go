package versionstore

import "github.com/kjelly/hufu/internal/sqlmigrate"

// migrations is the complete workspace_versions.db schema. Triggers enforce
// the snapshot invariants at the storage layer as well: immutable identity
// fields (I1), the allowed state transitions, and heads that only point at
// published snapshots of the same workspace and branch (I2).
var migrations = []sqlmigrate.Migration{
	{
		Version: 1,
		Name:    "initial_workspace_versions",
		SQL: `CREATE TABLE snapshots (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    branch_id TEXT NOT NULL,
    parent_snapshot_id TEXT REFERENCES snapshots(id),
    root_tree_hash TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    file_count INTEGER NOT NULL,
    logical_bytes INTEGER NOT NULL,
    materializable INTEGER NOT NULL CHECK (materializable IN (0,1)),
    reason TEXT NOT NULL,
    run_id TEXT,
    task_id TEXT,
    attempt INTEGER,
    anchor_event_id TEXT,
    commit_event_id TEXT UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('pending','published','orphaned','pruned')),
    created_at INTEGER NOT NULL,
    published_at INTEGER,
    pruned_at INTEGER
);
CREATE INDEX idx_snapshots_workspace_branch_created ON snapshots(workspace_id, branch_id, created_at);
CREATE INDEX idx_snapshots_parent ON snapshots(parent_snapshot_id);

CREATE TRIGGER snapshots_identity_immutable
BEFORE UPDATE OF id, workspace_id, branch_id, parent_snapshot_id, root_tree_hash, manifest_digest,
    file_count, logical_bytes, materializable, reason, run_id, task_id, attempt, anchor_event_id, created_at
ON snapshots
BEGIN
    SELECT RAISE(ABORT, 'workspace snapshot identity is immutable');
END;

CREATE TRIGGER snapshots_state_transition
BEFORE UPDATE OF state ON snapshots
WHEN NOT (
    (OLD.state = 'pending' AND NEW.state IN ('published','orphaned'))
    OR (OLD.state = 'published' AND NEW.state = 'pruned')
    OR OLD.state = NEW.state
)
BEGIN
    SELECT RAISE(ABORT, 'invalid workspace snapshot state transition');
END;

CREATE TABLE branch_heads (
    workspace_id TEXT NOT NULL,
    branch_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL REFERENCES snapshots(id),
    generation INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(workspace_id, branch_id)
);

CREATE TRIGGER branch_heads_insert_published
BEFORE INSERT ON branch_heads
WHEN NOT EXISTS (
    SELECT 1 FROM snapshots
    WHERE id = NEW.snapshot_id AND state = 'published'
      AND workspace_id = NEW.workspace_id AND branch_id = NEW.branch_id
)
BEGIN
    SELECT RAISE(ABORT, 'workspace branch head must reference a published snapshot of the same branch');
END;

CREATE TRIGGER branch_heads_update_published
BEFORE UPDATE OF snapshot_id ON branch_heads
WHEN NOT EXISTS (
    SELECT 1 FROM snapshots
    WHERE id = NEW.snapshot_id AND state = 'published'
      AND workspace_id = NEW.workspace_id AND branch_id = NEW.branch_id
)
BEGIN
    SELECT RAISE(ABORT, 'workspace branch head must reference a published snapshot of the same branch');
END;

CREATE TABLE operations (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    branch_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('capture','fork','checkout','restore','materialize','adopt','downgrade','gc')),
    state TEXT NOT NULL CHECK (state IN ('prepared','objects_written','event_committed','materializing','materialized','projection_updated','completed','failed')),
    from_snapshot_id TEXT,
    to_snapshot_id TEXT,
    idempotency_key TEXT,
    target_branch_id TEXT,
    detail_code TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX idx_operations_incomplete ON operations(state, updated_at);

CREATE TABLE subject_state (
    subject_key TEXT PRIMARY KEY,
    subject_root TEXT NOT NULL,
    mode_floor TEXT NOT NULL CHECK (mode_floor IN ('off','observe','required')),
    materialized_workspace_id TEXT,
    materialized_branch_id TEXT,
    materialized_snapshot_id TEXT REFERENCES snapshots(id),
    recovery_required INTEGER NOT NULL DEFAULT 0 CHECK (recovery_required IN (0,1)),
    recovery_code TEXT NOT NULL DEFAULT '',
    recovery_detail TEXT NOT NULL DEFAULT '',
    checkpoint_deferred INTEGER NOT NULL DEFAULT 0 CHECK (checkpoint_deferred IN (0,1)),
    generation INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE stat_cache (
    subject_key TEXT NOT NULL,
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime_ns INTEGER NOT NULL,
    ctime_ns INTEGER NOT NULL,
    inode INTEGER NOT NULL,
    mode INTEGER NOT NULL,
    blob_hash TEXT NOT NULL,
    PRIMARY KEY(subject_key, path)
);`,
	},
}
