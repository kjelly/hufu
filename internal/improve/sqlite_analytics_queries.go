package improve

// SQL aggregation over the TEMP analytics schema loaded by
// sqlite_analytics_loader.go, reproducing the legacy Go aggregation in
// improve.go (selectRecentRuns, summarizeTasks, collectExecutionMetrics).
// Every query here is covered by a parity test in
// sqlite_analytics_queries_test.go asserting it returns exactly what the
// legacy function returns for the same fixture — spec.md §14.2.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// sqlRunSummary is one row of the run-level view legacy selectRecentRuns
// builds in Go: a run's resolved team (the team of its first qualifying
// event, in file order — legacy never re-checks team on later events for
// the same run) and its [start, end] window over parseable timestamps only.
type sqlRunSummary struct {
	RunID       string
	Team        string
	StartUnixNS sql.NullInt64
	EndUnixNS   sql.NullInt64
}

// selectedRunsQuery reproduces the run grouping selectRecentRuns performs in
// Go: events with an empty run_id or empty team never contribute to a run at
// all (not even to Events), a run's Team is fixed to whichever qualifying
// event has the smallest event_seq, and Start/End only consider events whose
// timestamp parsed successfully. SQLite's default NULL ordering (NULLs
// first in ASC) makes a run with zero parseable timestamps sort as the
// earliest possible run without a sentinel value, matching Go's zero
// time.Time behaving as "before everything".
const selectedRunsQuery = `
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
)
SELECT rt.run_id, rt.team, rw.start_ns, rw.end_ns
FROM run_team rt
LEFT JOIN run_window rw ON rw.run_id = rt.run_id
ORDER BY rw.end_ns ASC, rt.run_id ASC`

func (s *sqliteAnalyticsSession) sqlAllRunSummaries(ctx context.Context) ([]sqlRunSummary, error) {
	rows, err := s.conn.QueryContext(ctx, selectedRunsQuery)
	if err != nil {
		return nil, fmt.Errorf("query run summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []sqlRunSummary
	for rows.Next() {
		var r sqlRunSummary
		if err := rows.Scan(&r.RunID, &r.Team, &r.StartUnixNS, &r.EndUnixNS); err != nil {
			return nil, fmt.Errorf("scan run summary: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run summaries: %w", err)
	}
	return out, nil
}

// sqlSelectRecentRuns is the SQL equivalent of selectRecentRuns: it resolves
// teamName (defaulting to the chronologically-last run's team when empty)
// and returns the up-to-runCount most recent run IDs for that team, oldest
// first — the exact order legacy's `selected` slice carries into
// flattenRuns/RunIDs/Trend.
func (s *sqliteAnalyticsSession) sqlSelectRecentRuns(ctx context.Context, teamName string, runCount int) (string, []string, error) {
	all, err := s.sqlAllRunSummaries(ctx)
	if err != nil {
		return "", nil, err
	}
	if teamName == "" && len(all) > 0 {
		teamName = all[len(all)-1].Team
	}
	selected := make([]string, 0, runCount)
	for _, r := range all {
		if r.Team == teamName {
			selected = append(selected, r.RunID)
		}
	}
	if len(selected) > runCount {
		selected = selected[len(selected)-runCount:]
	}
	return teamName, selected, nil
}

// runsInClause builds a parameter-bound `run_id IN (?, ?, ...)` fragment.
// Run IDs are always passed as bound parameters, never string-concatenated
// (spec.md §20.2's no-string-concatenation rule applied defensively even
// though run IDs are not external file paths).
func runsInClause(runIDs []string) (string, []any) {
	placeholders := make([]string, len(runIDs))
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

// sqlEventWindow reproduces eventWindow(events): the [min, max] over every
// parseable timestamp among events in scope, independent of task_id.
func (s *sqliteAnalyticsSession) sqlEventWindow(ctx context.Context, runIDs []string) (time.Time, time.Time, error) {
	if len(runIDs) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	inClause, args := runsInClause(runIDs)
	query := fmt.Sprintf(`SELECT MIN(timestamp_unix_ns), MAX(timestamp_unix_ns) FROM execution_events WHERE run_id IN (%s)`, inClause)
	var startNS, endNS sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, query, args...).Scan(&startNS, &endNS); err != nil {
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

// sqlTaskSummary mirrors taskSummary. Rows come from the task_summary /
// task_skills TEMP tables materialized once per session by
// materializeTaskViews (sqlite_analytics_task_summary.go), filtered by
// run_id at query time.
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

const taskSummaryQueryTemplate = `
SELECT run_id, task_id, agent, model, task_type, terminal, attempts, total_attempts, total_tokens
FROM task_summary
WHERE run_id IN (%s)
ORDER BY run_id ASC, task_id ASC`

const taskSkillsQueryTemplate = `
SELECT run_id, task_id, skill
FROM task_skills
WHERE run_id IN (%s)
ORDER BY run_id ASC, task_id ASC, skill ASC`

// sqlTaskSummaries reads the materialized task_summary/task_skills tables
// for the given run scope. materializeTaskViews must already have been
// called on this session (once, regardless of how many different run
// scopes are queried afterward).
func (s *sqliteAnalyticsSession) sqlTaskSummaries(ctx context.Context, runIDs []string) ([]sqlTaskSummary, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
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

	inClause, args := runsInClause(runIDs)

	rows, err := s.conn.QueryContext(ctx, fmt.Sprintf(taskSummaryQueryTemplate, inClause), args...)
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

	skillRows, err := s.conn.QueryContext(ctx, fmt.Sprintf(taskSkillsQueryTemplate, inClause), args...)
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

// sqlTokensByAgent reproduces the per-event agent attribution
// collectExecutionMetrics uses for Metrics.TokensByAgent. This is
// deliberately *not* the same grouping as GroupedMetrics.ByAgent (WP-4),
// which attributes a task's whole TotalTokens to the task's resolved
// (last-non-empty) agent instead: TokensByAgent sums each event's own
// Usage.TotalTokens under that event's own Agent field (falling back to
// "unspecified" per event, not per task).
const tokensByAgentQueryTemplate = `
SELECT CASE WHEN agent = '' THEN 'unspecified' ELSE agent END AS agent_key, SUM(total_tokens)
FROM execution_events
WHERE task_id <> '' AND run_id IN (%s)
GROUP BY agent_key`

func (s *sqliteAnalyticsSession) sqlTokensByAgent(ctx context.Context, runIDs []string) (map[string]int, error) {
	result := map[string]int{}
	if len(runIDs) == 0 {
		return result, nil
	}
	inClause, args := runsInClause(runIDs)
	rows, err := s.conn.QueryContext(ctx, fmt.Sprintf(tokensByAgentQueryTemplate, inClause), args...)
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

// sqlCollectExecutionMetrics is the SQL equivalent of collectExecutionMetrics,
// scoped to the given run IDs (already team/recency-filtered by
// sqlSelectRecentRuns). It leaves ToolCalls*/ToolErrors*/Memory* fields
// zero — those are populated by collectAuditMetrics (WP-5) and
// collectMemoryMetrics (WP-7/WP-8) respectively, exactly as legacy
// collectMetrics composes them.
func (s *sqliteAnalyticsSession) sqlCollectExecutionMetrics(ctx context.Context, runIDs []string) (Metrics, error) {
	metrics := Metrics{TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}

	metrics.RunCount = len(runIDs)
	if metrics.RunCount == 1 {
		metrics.RunID = runIDs[0]
	}

	start, end, err := s.sqlEventWindow(ctx, runIDs)
	if err != nil {
		return Metrics{}, err
	}
	metrics.StartedAt, metrics.EndedAt = start.Format(time.RFC3339), end.Format(time.RFC3339)

	tasks, err := s.sqlTaskSummaries(ctx, runIDs)
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

	tokensByAgent, err := s.sqlTokensByAgent(ctx, runIDs)
	if err != nil {
		return Metrics{}, err
	}
	metrics.TokensByAgent = tokensByAgent
	for _, tokens := range tokensByAgent {
		metrics.TotalTokens += tokens
	}

	return metrics, nil
}
