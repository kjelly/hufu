package improve

// TEMP analytics schema (spec.md §6). Every table here is created with
// CREATE TEMP TABLE: it lives only for the lifetime of the pinned connection
// in sqliteAnalyticsSession and is never persisted to context.sqlite or any
// other file on disk.

import (
	"context"
	"fmt"
)

// executionEventsSchema mirrors the fields of team.ExecutionEvent that
// existing metrics actually consume (spec.md §6.1). event_seq is a
// monotonically increasing surrogate key assigned at load time in JSONL file
// order; it exists purely to give window functions and ORDER BY a
// deterministic tie-breaker when two events share a timestamp (or have no
// parseable timestamp at all), matching the ordering Go slices already give
// legacy aggregation for free.
const executionEventsSchema = `
CREATE TEMP TABLE execution_events (
    event_seq            INTEGER PRIMARY KEY,
    version              INTEGER NOT NULL,
    timestamp_raw        TEXT NOT NULL,
    timestamp_unix_ns    INTEGER,
    run_id               TEXT NOT NULL,
    team                 TEXT NOT NULL,
    task_id              TEXT NOT NULL DEFAULT '',
    agent                TEXT NOT NULL DEFAULT '',
    attempt              INTEGER NOT NULL DEFAULT 0,
    status               TEXT NOT NULL DEFAULT '',
    model                TEXT NOT NULL DEFAULT '',
    task_type            TEXT NOT NULL DEFAULT '',
    skills_reported      INTEGER NOT NULL DEFAULT 0,
    team_revision        TEXT NOT NULL DEFAULT '',
    duration_ms          INTEGER NOT NULL DEFAULT 0,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    total_tokens          INTEGER NOT NULL DEFAULT 0,
    progress_tokens       INTEGER NOT NULL DEFAULT 0,
    outcome               TEXT NOT NULL DEFAULT '',
    stop_reason           TEXT NOT NULL DEFAULT '',
    acceptance_state      TEXT NOT NULL DEFAULT '',
    repair_attempts       INTEGER NOT NULL DEFAULT 0,
    phase                 TEXT NOT NULL DEFAULT '',
    provider              TEXT NOT NULL DEFAULT '',
    failure_signature     TEXT NOT NULL DEFAULT ''
);`

// executionEventSkillsSchema normalizes ExecutionEvent.Skills so grouping by
// skill never needs to re-parse a JSON array (spec.md §6.2). One row per
// (event, skill) pair; WP-4's task-level skill query must reproduce legacy's
// per-task *overwrite* semantics (see parity_test.go
// TestSummarizeTasks_SkillsFieldOverwritesRatherThanUnionsAcrossEvents), not
// a plain DISTINCT union across every event ever seen for the task.
const executionEventSkillsSchema = `
CREATE TEMP TABLE execution_event_skills (
    event_seq INTEGER NOT NULL,
    run_id    TEXT NOT NULL,
    task_id   TEXT NOT NULL,
    skill     TEXT NOT NULL,
    PRIMARY KEY (event_seq, skill)
);`

// auditEventsSchema only stores what collectAuditMetrics-equivalent SQL
// needs: timestamp, team, agent, and event type. It must never gain a
// payload/input/command column (spec.md §6.3, §20.1) — audit JSON can
// contain secrets, and today's report contract guarantees they never reach
// the report.
const auditEventsSchema = `
CREATE TEMP TABLE audit_events (
    event_seq         INTEGER PRIMARY KEY,
    timestamp_raw     TEXT NOT NULL,
    timestamp_unix_ns INTEGER,
    team              TEXT NOT NULL DEFAULT '',
    agent             TEXT NOT NULL DEFAULT '',
    event             TEXT NOT NULL DEFAULT ''
);`

// memoryEventsSchema stores only the event identity and scalar fields derived
// from memory-event payloads. The raw RunEvent payload is intentionally not a
// column: it may contain memory content or context summaries and is needed
// only transiently while the streaming loader decodes each event.
const memoryEventsSchema = `
CREATE TEMP TABLE memory_events (
    event_seq         INTEGER PRIMARY KEY,
    run_id            TEXT NOT NULL DEFAULT '',
    task_id           TEXT NOT NULL DEFAULT '',
    attempt           INTEGER NOT NULL DEFAULT 0,
    type              TEXT NOT NULL DEFAULT '',
    timestamp_raw     TEXT NOT NULL DEFAULT '',
    timestamp_unix_ns INTEGER,
    retrieval_id      TEXT NOT NULL DEFAULT '',
    reason_code       TEXT NOT NULL DEFAULT '',
    token_count       INTEGER NOT NULL DEFAULT 0,
    disposition       TEXT NOT NULL DEFAULT '',
    signal            TEXT NOT NULL DEFAULT '',
    direction         TEXT NOT NULL DEFAULT ''
);`

// analyticsSchemaStatements build the TEMP schema for a fresh session.
// Indexes are deliberately not part of this list — spec.md §7 requires
// building them only after bulk load so per-row INSERTs during ingestion
// never pay index-maintenance cost.
var analyticsSchemaStatements = []string{
	executionEventsSchema,
	executionEventSkillsSchema,
	auditEventsSchema,
	memoryEventsSchema,
}

func (s *sqliteAnalyticsSession) createSchema(ctx context.Context) error {
	for _, stmt := range analyticsSchemaStatements {
		if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create analytics schema: %w", err)
		}
	}
	return nil
}

// analyticsIndexStatements are created once bulk ingestion completes
// (spec.md §7). Building them here rather than in the CREATE TABLE
// statements keeps the ingestion loader free to bulk-insert without paying
// per-row index maintenance.
var analyticsIndexStatements = []string{
	`CREATE INDEX temp.idx_execution_team_run ON execution_events(team, run_id, event_seq)`,
	`CREATE INDEX temp.idx_execution_task_attempt ON execution_events(run_id, task_id, attempt, event_seq)`,
	`CREATE INDEX temp.idx_execution_agent ON execution_events(agent, run_id)`,
	`CREATE INDEX temp.idx_execution_model ON execution_events(model, run_id)`,
	`CREATE INDEX temp.idx_execution_task_type ON execution_events(task_type, run_id)`,
	`CREATE INDEX temp.idx_skill_task ON execution_event_skills(skill, run_id, task_id)`,
	`CREATE INDEX temp.idx_audit_team_time ON audit_events(team, timestamp_unix_ns, agent, event)`,
	`CREATE INDEX temp.idx_memory_type_run ON memory_events(type, run_id, event_seq)`,
}

// createIndexes builds every analytics index. Safe to call once, after all
// ingestion for the session has finished.
func (s *sqliteAnalyticsSession) createIndexes(ctx context.Context) error {
	for _, stmt := range analyticsIndexStatements {
		if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create analytics index: %w", err)
		}
	}
	return nil
}
