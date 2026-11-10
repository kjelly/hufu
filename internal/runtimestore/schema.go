package runtimestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kjelly/hufu/internal/sqlmigrate"
)

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
  name TEXT NOT NULL,
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
	{Version: 2, Name: "decision index", SQL: `
CREATE TABLE decision_index (
  sequence INTEGER PRIMARY KEY,
  decision_id TEXT NOT NULL,
  entry BLOB NOT NULL
);
CREATE INDEX decision_index_decision ON decision_index(decision_id, sequence);`},
	// Appends only insert events. Any update or delete is a rewrite of the
	// validated journal; the counter lets an event store that validated the
	// journal notice one with a single read and revalidate everything.
	{Version: 3, Name: "event journal rewrite counter", SQL: `
CREATE TABLE event_journal_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  rewrites INTEGER NOT NULL
);
INSERT INTO event_journal_meta(id, rewrites) VALUES(1, 0);
CREATE TRIGGER events_rewritten_by_update AFTER UPDATE ON events
BEGIN UPDATE event_journal_meta SET rewrites = rewrites + 1 WHERE id = 1; END;
CREATE TRIGGER events_rewritten_by_delete AFTER DELETE ON events
BEGIN UPDATE event_journal_meta SET rewrites = rewrites + 1 WHERE id = 1; END;`},
	{Version: 4, Name: "immutable run report snapshots", SQL: `
CREATE TABLE run_reports (
  source_event_id TEXT PRIMARY KEY REFERENCES events(id),
  source_hash TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('available', 'unavailable')),
  reason TEXT NOT NULL DEFAULT '',
  snapshot BLOB
);`},
	{Version: 5, Name: "historical result grant cycle references", SQL: `
CREATE TABLE historical_grant_cycles (
  branch_id TEXT NOT NULL,
  cycle_id TEXT NOT NULL,
  source_event_id TEXT NOT NULL UNIQUE REFERENCES events(id),
  source_hash TEXT NOT NULL,
  PRIMARY KEY (branch_id, cycle_id)
);`},
	{Version: 6, Name: "derived fork branch intent", SQL: `
ALTER TABLE session_branches ADD COLUMN derivation BLOB;
CREATE UNIQUE INDEX events_derived_fork ON events(branch_id) WHERE type = 'derived_fork_created';`},
}

func verifyReadOnlySchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read runtime store schema version: %w", err)
	}
	required := migrations
	if version >= 3 && version < len(migrations) {
		// Preserve the previously supported read-only formats. Missing new
		// grant references mean no grant, never an inferred authorization.
		// Every existing migration still needs its original checksum/name.
		required = migrations[:version]
	}
	return sqlmigrate.Verify(ctx, db, "runtime store", required)
}
