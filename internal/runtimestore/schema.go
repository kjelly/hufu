package runtimestore

import "github.com/kjelly/hufu/internal/sqlmigrate"

// migrations is the runtime.sqlite schema. Applied migrations are checksum
// verified; an edited, renamed, or unknown migration fails closed.
var migrations = []sqlmigrate.Migration{
	{Version: 1, Name: "event journal and runtime documents", SQL: `
CREATE TABLE events (
  sequence INTEGER PRIMARY KEY,
  id TEXT NOT NULL UNIQUE,
  schema_version INTEGER NOT NULL,
  previous_id TEXT NOT NULL DEFAULT '',
  invocation_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  branch_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  actor TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  timestamp TEXT NOT NULL DEFAULT '',
  idempotency_scope TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL DEFAULT '',
  payload BLOB,
  previous_hash TEXT NOT NULL DEFAULT '',
  hash TEXT NOT NULL
);
CREATE UNIQUE INDEX events_idempotency ON events(idempotency_scope, idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX events_run ON events(run_id, sequence);
CREATE TABLE runtime_documents (
  name TEXT PRIMARY KEY,
  body BLOB NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE task_journal (
  sequence INTEGER PRIMARY KEY,
  recorded_at TEXT NOT NULL,
  record BLOB NOT NULL
);
CREATE INDEX events_branch ON events(branch_id, sequence);
CREATE TABLE session_branches (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  parent_id TEXT NOT NULL DEFAULT '',
  fork_event_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  state BLOB NOT NULL
);
CREATE TABLE session_labels (
  name TEXT PRIMARY KEY,
  target TEXT NOT NULL
);
CREATE TABLE session_tree_head (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  active_branch TEXT NOT NULL
);`},
}
