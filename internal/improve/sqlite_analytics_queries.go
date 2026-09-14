package improve

// SQL aggregation over the TEMP analytics schema loaded by
// sqlite_analytics_loader.go, preserving the public execution metrics
// semantics without materializing the complete event history.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// selectedRun is one row in the frozen selected_runs scope.
type selectedRun struct {
	RunID       string
	Ordinal     int
	Team        string
	StartUnixNS sql.NullInt64
	EndUnixNS   sql.NullInt64
}

// runSummaryCTE is the single semantic source for selection. A run's team is
// its first qualifying event's team, and its time window ignores timestamps
// that did not parse.
const runSummaryCTE = `
WITH qualifying AS (
    SELECT event_seq, run_id, team, timestamp_unix_ns
    FROM execution_events
    WHERE run_id <> '' AND team <> ''
),
run_team AS (
    SELECT run_id, team FROM (
        SELECT run_id, team,
               ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY event_seq ASC) AS rn
        FROM qualifying
    ) WHERE rn = 1
),
run_window AS (
    SELECT run_id, MIN(timestamp_unix_ns) AS start_ns, MAX(timestamp_unix_ns) AS end_ns
    FROM qualifying
    WHERE timestamp_unix_ns IS NOT NULL
    GROUP BY run_id
),
run_summary AS (
    SELECT rt.run_id, rt.team, rw.start_ns, rw.end_ns
    FROM run_team rt
    LEFT JOIN run_window rw ON rw.run_id = rt.run_id
)
`

const defaultTeamQuery = runSummaryCTE + `
SELECT team
FROM run_summary
ORDER BY (end_ns IS NULL) ASC, end_ns DESC, run_id DESC
LIMIT 1`

const recentRunsQuery = runSummaryCTE + `
SELECT run_id, team, start_ns, end_ns
FROM (
    SELECT run_id, team, start_ns, end_ns
    FROM run_summary
    WHERE team = ?
    ORDER BY (end_ns IS NULL) ASC, end_ns DESC, run_id DESC
    LIMIT ?
)
ORDER BY (end_ns IS NOT NULL) ASC, end_ns ASC, run_id ASC`

const insertSelectedRunSQL = `
INSERT INTO selected_runs (run_id, ordinal, team, start_ns, end_ns)
VALUES (?, ?, ?, ?, ?)`

// sqlSelectRecentRuns resolves
// teamName (defaulting to the chronologically-last run's team when empty)
// and returns the up-to-runCount most recent run IDs for that team, oldest
// first — the exact order carried into
// run IDs and trend ordering.
func (s *sqliteAnalyticsSession) sqlSelectRecentRuns(ctx context.Context, teamName string, runCount int) (string, []string, error) {
	teamName, selected, err := s.sqlSelectRecentRunSummaries(ctx, teamName, runCount)
	if err != nil {
		return "", nil, err
	}
	runIDs := make([]string, len(selected))
	for i, run := range selected {
		runIDs[i] = run.RunID
	}
	return teamName, runIDs, nil
}

func (s *sqliteAnalyticsSession) sqlSelectRecentRunSummaries(ctx context.Context, teamName string, runCount int) (string, []selectedRun, error) {
	if s.selectedRunsReady {
		return "", nil, fmt.Errorf("run selection already completed")
	}
	if runCount < 1 {
		return "", nil, fmt.Errorf("run count must be at least 1")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("begin run selection transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if teamName == "" {
		if err := tx.QueryRowContext(ctx, defaultTeamQuery).Scan(&teamName); err != nil && err != sql.ErrNoRows {
			return "", nil, fmt.Errorf("resolve default team: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, recentRunsQuery, teamName, runCount)
	if err != nil {
		return "", nil, fmt.Errorf("query recent runs: %w", err)
	}
	selected := make([]selectedRun, 0, runCount)
	for rows.Next() {
		var run selectedRun
		if err := rows.Scan(&run.RunID, &run.Team, &run.StartUnixNS, &run.EndUnixNS); err != nil {
			_ = rows.Close()
			return "", nil, fmt.Errorf("scan recent run: %w", err)
		}
		run.Ordinal = len(selected)
		selected = append(selected, run)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", nil, fmt.Errorf("iterate recent runs: %w", err)
	}
	_ = rows.Close()
	for _, run := range selected {
		if _, err := tx.ExecContext(ctx, insertSelectedRunSQL, run.RunID, run.Ordinal, run.Team, run.StartUnixNS, run.EndUnixNS); err != nil {
			return "", nil, fmt.Errorf("insert selected run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("commit run selection: %w", err)
	}
	committed = true
	s.selectedRunsReady = true
	return teamName, selected, nil
}

func (s *sqliteAnalyticsSession) sqlSelectedRuns(ctx context.Context) ([]selectedRun, error) {
	if !s.selectedRunsReady {
		return nil, fmt.Errorf("run selection has not completed")
	}
	rows, err := s.executor.QueryContext(ctx, `
SELECT run_id, ordinal, team, start_ns, end_ns
FROM selected_runs
ORDER BY ordinal`)
	if err != nil {
		return nil, fmt.Errorf("query selected runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var selected []selectedRun
	for rows.Next() {
		var run selectedRun
		if err := rows.Scan(&run.RunID, &run.Ordinal, &run.Team, &run.StartUnixNS, &run.EndUnixNS); err != nil {
			return nil, fmt.Errorf("scan selected run: %w", err)
		}
		selected = append(selected, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate selected runs: %w", err)
	}
	return selected, nil
}

// sqlEventWindow computes the [min, max] over every
// parseable timestamp among events in scope, independent of task_id.
func (s *sqliteAnalyticsSession) sqlEventWindow(ctx context.Context) (time.Time, time.Time, error) {
	const query = `
SELECT MIN(start_ns), MAX(end_ns)
FROM selected_runs`
	var startNS, endNS sql.NullInt64
	if err := s.executor.QueryRowContext(ctx, query).Scan(&startNS, &endNS); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("query event window: %w", err)
	}
	return unixNSToTime(startNS), unixNSToTime(endNS), nil
}

func unixNSToTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(0, v.Int64).UTC()
}

// sqlTaskSummary is the task-level projection. Rows come from the task_summary /
// task_skills TEMP tables materialized once per session by
// materializeTaskViews (sqlite_analytics_task_summary.go). An ordinal can
// narrow reads to one member of the selected scope for legacy trend queries.
type sqlTaskSummary struct {
	RunID         string
	TaskID        string
	Agent         string
	Model         string
	TaskType      string
	Skills        []string
	Terminal      string
	Attempts      int
	TotalAttempts int
	TotalTokens   int
}

const taskSummaryQuery = `
SELECT t.run_id, t.task_id, t.agent, t.model, t.task_type, t.terminal, t.attempts, t.total_attempts, t.total_tokens
FROM task_summary t
JOIN selected_runs sr ON sr.run_id = t.run_id
ORDER BY sr.ordinal ASC, t.task_id ASC`

const taskSkillsQuery = `
SELECT ts.run_id, ts.task_id, ts.skill
FROM task_skills ts
JOIN selected_runs sr ON sr.run_id = ts.run_id
ORDER BY sr.ordinal ASC, ts.task_id ASC, ts.skill ASC`

// sqlTaskSummaries reads the selected-only task_summary/task_skills tables.
func (s *sqliteAnalyticsSession) sqlTaskSummaries(ctx context.Context) ([]sqlTaskSummary, error) {
	if err := s.ensureTaskViews(ctx); err != nil {
		return nil, err
	}
	tasks := make(map[[2]string]*sqlTaskSummary)
	order := make([]([2]string), 0)
	get := func(runID, taskID string) *sqlTaskSummary {
		key := [2]string{runID, taskID}
		t := tasks[key]
		if t == nil {
			t = &sqlTaskSummary{RunID: runID, TaskID: taskID}
			tasks[key] = t
			order = append(order, key)
		}
		return t
	}

	rows, err := s.executor.QueryContext(ctx, taskSummaryQuery)
	if err != nil {
		return nil, fmt.Errorf("query task summary: %w", err)
	}
	for rows.Next() {
		var runID, taskID, agent, model, taskType, terminal string
		var attempts, totalAttempts, totalTokens int
		if err := rows.Scan(&runID, &taskID, &agent, &model, &taskType, &terminal, &attempts, &totalAttempts, &totalTokens); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan task summary: %w", err)
		}
		t := get(runID, taskID)
		t.Agent, t.Model, t.TaskType, t.Terminal = agent, model, taskType, terminal
		t.Attempts, t.TotalAttempts, t.TotalTokens = attempts, totalAttempts, totalTokens
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate task summary: %w", err)
	}
	_ = rows.Close()

	skillRows, err := s.executor.QueryContext(ctx, taskSkillsQuery)
	if err != nil {
		return nil, fmt.Errorf("query task skills: %w", err)
	}
	for skillRows.Next() {
		var runID, taskID, skill string
		if err := skillRows.Scan(&runID, &taskID, &skill); err != nil {
			_ = skillRows.Close()
			return nil, fmt.Errorf("scan task skill: %w", err)
		}
		t := get(runID, taskID)
		t.Skills = append(t.Skills, skill)
	}
	if err := skillRows.Err(); err != nil {
		_ = skillRows.Close()
		return nil, fmt.Errorf("iterate task skills: %w", err)
	}
	_ = skillRows.Close()

	out := make([]sqlTaskSummary, 0, len(order))
	for _, key := range order {
		out = append(out, *tasks[key])
	}
	return out, nil
}

// sqlSelectedRevisions returns the sorted overall revision set and the latest
// non-empty revision for each selected ordinal without projecting execution
// rows into Go.
func (s *sqliteAnalyticsSession) sqlSelectedRevisions(ctx context.Context) ([]string, map[int]string, error) {
	const overallQuery = `
SELECT DISTINCT e.team_revision
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
WHERE e.team <> '' AND e.team_revision <> ''
ORDER BY e.team_revision`
	overall, err := queryStrings(ctx, s, overallQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("query selected team revisions: %w", err)
	}
	const latestByRunQuery = `
WITH ranked AS (
    SELECT sr.ordinal, e.team_revision,
           ROW_NUMBER() OVER (PARTITION BY sr.ordinal ORDER BY e.event_seq DESC) AS rank
    FROM selected_runs sr
    JOIN execution_events e ON e.run_id = sr.run_id
    WHERE e.team <> '' AND e.team_revision <> ''
)
SELECT ordinal, team_revision
FROM ranked
WHERE rank = 1
ORDER BY ordinal`
	rows, err := s.executor.QueryContext(ctx, latestByRunQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("query latest team revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byOrdinal := make(map[int]string)
	for rows.Next() {
		var ordinal int
		var revision string
		if err := rows.Scan(&ordinal, &revision); err != nil {
			return nil, nil, fmt.Errorf("scan latest team revision: %w", err)
		}
		byOrdinal[ordinal] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate latest team revisions: %w", err)
	}
	return overall, byOrdinal, nil
}

// sqlTokensByAgent provides the per-event agent attribution
// used for Metrics.TokensByAgent. This is
// deliberately *not* the same grouping as GroupedMetrics.ByAgent (WP-4),
// which attributes a task's whole TotalTokens to the task's resolved
// (last-non-empty) agent instead: TokensByAgent sums each event's own
// Usage.TotalTokens under that event's own Agent field (falling back to
// "unspecified" per event, not per task).
const tokensByAgentQuery = `
SELECT CASE WHEN e.agent = '' THEN 'unspecified' ELSE e.agent END AS agent_key, SUM(e.total_tokens)
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
WHERE e.task_id <> '' AND e.team <> ''
GROUP BY agent_key`

func (s *sqliteAnalyticsSession) sqlTokensByAgent(ctx context.Context) (map[string]int, error) {
	result := map[string]int{}
	rows, err := s.executor.QueryContext(ctx, tokensByAgentQuery)
	if err != nil {
		return nil, fmt.Errorf("query tokens by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var agent string
		var tokens int
		if err := rows.Scan(&agent, &tokens); err != nil {
			return nil, fmt.Errorf("scan tokens by agent: %w", err)
		}
		result[agent] = tokens
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tokens by agent: %w", err)
	}
	return result, nil
}

// sqlCollectExecutionMetrics performs execution aggregation,
// scoped to all selected runs, or one selected ordinal for a trend point.
// It leaves ToolCalls*/ToolErrors*/Memory* fields
// zero — those are populated by the audit and memory SQL aggregations,
// respectively, exactly as the public metrics contract composes them.
func (s *sqliteAnalyticsSession) sqlCollectExecutionMetrics(ctx context.Context) (Metrics, error) {
	metrics := Metrics{TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	selected, err := s.sqlSelectedRuns(ctx)
	if err != nil {
		return Metrics{}, err
	}
	metrics.RunCount = len(selected)
	if metrics.RunCount == 1 {
		metrics.RunID = selected[0].RunID
	}

	start, end, err := s.sqlEventWindow(ctx)
	if err != nil {
		return Metrics{}, err
	}
	metrics.StartedAt, metrics.EndedAt = start.Format(time.RFC3339), end.Format(time.RFC3339)

	tasks, err := s.sqlTaskSummaries(ctx)
	if err != nil {
		return Metrics{}, err
	}
	metrics.TotalTasks = len(tasks)
	for _, task := range tasks {
		metrics.TotalAttempts += task.TotalAttempts
		if task.Attempts > 1 {
			metrics.RetriedTasks++
		}
		switch task.Terminal {
		case "done":
			metrics.Done++
		case "error":
			metrics.Error++
		case "planned":
			metrics.Planned++
		}
	}

	tokensByAgent, err := s.sqlTokensByAgent(ctx)
	if err != nil {
		return Metrics{}, err
	}
	metrics.TokensByAgent = tokensByAgent
	for _, tokens := range tokensByAgent {
		metrics.TotalTokens += tokens
	}

	return metrics, nil
}
