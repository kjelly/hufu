package improve

// task_summary / task_skills are TEMP tables materialized once per session,
// right after execution events finish loading, from the same
// last-non-empty-field / terminal-status / last-skill-event window-function
// queries WP-3 introduced. Materializing them lets both
// sqlCollectExecutionMetrics (WP-3) and sqlCollectGroupedMetrics (WP-4)
// share one SQL GROUP BY-friendly source instead of each re-deriving task
// state, and matches the shared "task_summary"/"task_skills" views spec.md
// §9.3/§9.4 describes — with task_skills reproducing legacy's
// overwrite-on-last-report semantics (see
// TestSummarizeTasks_SkillsFieldOverwritesRatherThanUnionsAcrossEvents),
// not spec.md's originally proposed plain DISTINCT union.

import (
	"context"
	"fmt"
)

const createTaskSummarySchema = `
CREATE TEMP TABLE task_summary (
    run_id         TEXT NOT NULL,
    task_id        TEXT NOT NULL,
    agent          TEXT NOT NULL DEFAULT '',
    model          TEXT NOT NULL DEFAULT '',
    task_type      TEXT NOT NULL DEFAULT '',
    terminal       TEXT NOT NULL DEFAULT '',
    attempts       INTEGER NOT NULL DEFAULT 0,
    total_attempts INTEGER NOT NULL DEFAULT 0,
    total_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, task_id)
);
CREATE TEMP TABLE task_skills (
    run_id  TEXT NOT NULL,
    task_id TEXT NOT NULL,
    skill   TEXT NOT NULL,
    PRIMARY KEY (run_id, task_id, skill)
);`

const populateTaskSummarySQL = `
INSERT INTO task_summary (run_id, task_id, agent, model, task_type, terminal, attempts, total_attempts, total_tokens)
WITH agg AS (
    SELECT run_id, task_id,
           MAX(0, MAX(attempt)) AS attempts,
           SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END) AS total_attempts,
           SUM(total_tokens) AS total_tokens
    FROM execution_events
    WHERE task_id <> ''
    GROUP BY run_id, task_id
),
last_agent AS (
    SELECT run_id, task_id, agent FROM (
        SELECT run_id, task_id, agent,
               ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
        FROM execution_events WHERE task_id <> '' AND agent <> ''
    ) WHERE rn = 1
),
last_model AS (
    SELECT run_id, task_id, model FROM (
        SELECT run_id, task_id, model,
               ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
        FROM execution_events WHERE task_id <> '' AND model <> ''
    ) WHERE rn = 1
),
last_task_type AS (
    SELECT run_id, task_id, task_type FROM (
        SELECT run_id, task_id, task_type,
               ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
        FROM execution_events WHERE task_id <> '' AND task_type <> ''
    ) WHERE rn = 1
),
last_terminal AS (
    SELECT run_id, task_id, status FROM (
        SELECT run_id, task_id, status,
               ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
        FROM execution_events WHERE task_id <> '' AND status IN ('done', 'error', 'planned')
    ) WHERE rn = 1
)
SELECT agg.run_id, agg.task_id,
       COALESCE(last_agent.agent, ''), COALESCE(last_model.model, ''), COALESCE(last_task_type.task_type, ''),
       COALESCE(last_terminal.status, ''),
       agg.attempts, agg.total_attempts, agg.total_tokens
FROM agg
LEFT JOIN last_agent ON last_agent.run_id = agg.run_id AND last_agent.task_id = agg.task_id
LEFT JOIN last_model ON last_model.run_id = agg.run_id AND last_model.task_id = agg.task_id
LEFT JOIN last_task_type ON last_task_type.run_id = agg.run_id AND last_task_type.task_id = agg.task_id
LEFT JOIN last_terminal ON last_terminal.run_id = agg.run_id AND last_terminal.task_id = agg.task_id`

const populateTaskSkillsSQL = `
INSERT INTO task_skills (run_id, task_id, skill)
WITH last_skill_event AS (
    SELECT run_id, task_id, event_seq FROM (
        SELECT te.run_id, te.task_id, te.event_seq,
               ROW_NUMBER() OVER (PARTITION BY te.run_id, te.task_id ORDER BY te.event_seq DESC) AS rn
        FROM execution_events te
        WHERE te.task_id <> ''
          AND te.skills_reported = 1
    ) WHERE rn = 1
)
SELECT lse.run_id, lse.task_id, s.skill
FROM last_skill_event lse
JOIN execution_event_skills s ON s.event_seq = lse.event_seq
ORDER BY lse.run_id ASC, lse.task_id ASC, s.skill ASC`

// materializeTaskViews builds task_summary and task_skills once for the
// session, over every task_id-bearing event currently loaded into
// execution_events — not scoped to any particular run selection. Every
// query against these tables filters by run_id at query time instead, so
// this only needs to run once no matter how many different run scopes
// (overall Metrics, per-run TrendPoint, GroupedMetrics) are queried
// afterward. Must be called after loadExecutionEvents and before any
// task_summary/task_skills query; calling it twice on the same session is
// rejected as a lifecycle error.
func (s *sqliteAnalyticsSession) materializeTaskViews(ctx context.Context) error {
	if s.taskViewsReady {
		return fmt.Errorf("task projections already materialized")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task projection transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, createTaskSummarySchema); err != nil {
		return fmt.Errorf("create task summary schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, populateTaskSummarySQL); err != nil {
		return fmt.Errorf("populate task summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, populateTaskSkillsSQL); err != nil {
		return fmt.Errorf("populate task skills: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task projections: %w", err)
	}
	committed = true
	s.taskViewsReady = true
	return nil
}

// ensureTaskViews materializes task_summary/task_skills at most once per
// session. Every query that reads either table must call this first instead
// of calling materializeTaskViews directly, so callers never have to track
// materialization order themselves.
func (s *sqliteAnalyticsSession) ensureTaskViews(ctx context.Context) error {
	if s.taskViewsReady {
		return nil
	}
	return s.materializeTaskViews(ctx)
}
