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

// sqlTaskSummary mirrors taskSummary but is populated entirely by SQL.
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

// taskAggregatesQuery computes the per-task aggregates that are simple
// GROUP BY reductions: MAX(attempt) (task.Attempts), a count of in_progress
// events (task.TotalAttempts), and SUM(total_tokens) over every event for
// the task (task.TotalTokens) — matching summarizeTasks exactly.
const taskAggregatesQueryTemplate = `
SELECT run_id, task_id,
       MAX(attempt) AS attempts,
       SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END) AS total_attempts,
       SUM(total_tokens) AS total_tokens
FROM execution_events
WHERE task_id <> '' AND run_id IN (%s)
GROUP BY run_id, task_id`

// lastNonEmptyFieldQueryTemplate finds, per task, the value of `field` on
// the event with the greatest event_seq among events where `field` is
// non-empty — i.e. "the last event that actually reported this field",
// reproducing summarizeTasks' `if event.X != "" { task.X = event.X }`
// overwrite-on-report loop without materializing every event in Go.
const lastNonEmptyFieldQueryTemplate = `
SELECT run_id, task_id, %[1]s FROM (
    SELECT run_id, task_id, %[1]s,
           ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
    FROM execution_events
    WHERE task_id <> '' AND %[1]s <> '' AND run_id IN (%[2]s)
) WHERE rn = 1`

// terminalStatusQuery finds, per task, the status of the last event (by
// event_seq) whose status is one of the three terminal states legacy
// recognizes.
const terminalStatusQueryTemplate = `
SELECT run_id, task_id, status FROM (
    SELECT run_id, task_id, status,
           ROW_NUMBER() OVER (PARTITION BY run_id, task_id ORDER BY event_seq DESC) AS rn
    FROM execution_events
    WHERE task_id <> '' AND status IN ('done', 'error', 'planned') AND run_id IN (%s)
) WHERE rn = 1`

// lastSkillEventQuery finds, per task, the event_seq of the last event (by
// event_seq) that reported at least one skill. Legacy overwrites
// task.Skills wholesale on every event carrying a non-empty Skills list
// (parity_test.go
// TestSummarizeTasks_SkillsFieldOverwritesRatherThanUnionsAcrossEvents) — it
// is not a union of every skill ever seen for the task, so this
// deliberately does *not* match spec.md §9.4's plain DISTINCT task_skills
// view.
const lastSkillEventQueryTemplate = `
SELECT run_id, task_id, event_seq FROM (
    SELECT te.run_id, te.task_id, te.event_seq,
           ROW_NUMBER() OVER (PARTITION BY te.run_id, te.task_id ORDER BY te.event_seq DESC) AS rn
    FROM execution_events te
    WHERE te.task_id <> '' AND te.run_id IN (%s)
      AND EXISTS (SELECT 1 FROM execution_event_skills s WHERE s.event_seq = te.event_seq)
) WHERE rn = 1`

func (s *sqliteAnalyticsSession) sqlTaskSummaries(ctx context.Context, runIDs []string) ([]sqlTaskSummary, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	tasks := make(map[[2]string]*sqlTaskSummary)
	get := func(runID, taskID string) *sqlTaskSummary {
		key := [2]string{runID, taskID}
		t := tasks[key]
		if t == nil {
			t = &sqlTaskSummary{RunID: runID, TaskID: taskID}
			tasks[key] = t
		}
		return t
	}

	inClause, args := runsInClause(runIDs)

	aggRows, err := s.conn.QueryContext(ctx, fmt.Sprintf(taskAggregatesQueryTemplate, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("query task aggregates: %w", err)
	}
	for aggRows.Next() {
		var runID, taskID string
		var attempts, totalAttempts, totalTokens int
		if err := aggRows.Scan(&runID, &taskID, &attempts, &totalAttempts, &totalTokens); err != nil {
			_ = aggRows.Close()
			return nil, fmt.Errorf("scan task aggregates: %w", err)
		}
		t := get(runID, taskID)
		t.Attempts, t.TotalAttempts, t.TotalTokens = attempts, totalAttempts, totalTokens
	}
	if err := aggRows.Err(); err != nil {
		_ = aggRows.Close()
		return nil, fmt.Errorf("iterate task aggregates: %w", err)
	}
	_ = aggRows.Close()

	for _, field := range []string{"agent", "model", "task_type"} {
		query := fmt.Sprintf(lastNonEmptyFieldQueryTemplate, field, inClause)
		rows, err := s.conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query last non-empty %s: %w", field, err)
		}
		for rows.Next() {
			var runID, taskID, value string
			if err := rows.Scan(&runID, &taskID, &value); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan last non-empty %s: %w", field, err)
			}
			t := get(runID, taskID)
			switch field {
			case "agent":
				t.Agent = value
			case "model":
				t.Model = value
			case "task_type":
				t.TaskType = value
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate last non-empty %s: %w", field, err)
		}
		_ = rows.Close()
	}

	terminalRows, err := s.conn.QueryContext(ctx, fmt.Sprintf(terminalStatusQueryTemplate, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("query terminal status: %w", err)
	}
	for terminalRows.Next() {
		var runID, taskID, status string
		if err := terminalRows.Scan(&runID, &taskID, &status); err != nil {
			_ = terminalRows.Close()
			return nil, fmt.Errorf("scan terminal status: %w", err)
		}
		get(runID, taskID).Terminal = status
	}
	if err := terminalRows.Err(); err != nil {
		_ = terminalRows.Close()
		return nil, fmt.Errorf("iterate terminal status: %w", err)
	}
	_ = terminalRows.Close()

	skillEventRows, err := s.conn.QueryContext(ctx, fmt.Sprintf(lastSkillEventQueryTemplate, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("query last skill event: %w", err)
	}
	type taskKey = [2]string
	skillEventSeqByTask := make(map[taskKey]int64)
	eventSeqToTask := make(map[int64]taskKey)
	for skillEventRows.Next() {
		var runID, taskID string
		var eventSeq int64
		if err := skillEventRows.Scan(&runID, &taskID, &eventSeq); err != nil {
			_ = skillEventRows.Close()
			return nil, fmt.Errorf("scan last skill event: %w", err)
		}
		key := taskKey{runID, taskID}
		skillEventSeqByTask[key] = eventSeq
		eventSeqToTask[eventSeq] = key
	}
	if err := skillEventRows.Err(); err != nil {
		_ = skillEventRows.Close()
		return nil, fmt.Errorf("iterate last skill event: %w", err)
	}
	_ = skillEventRows.Close()

	if len(eventSeqToTask) > 0 {
		eventSeqs := make([]int64, 0, len(eventSeqToTask))
		for seq := range eventSeqToTask {
			eventSeqs = append(eventSeqs, seq)
		}
		seqClause, seqArgs := int64InClause(eventSeqs)
		query := fmt.Sprintf(`SELECT event_seq, skill FROM execution_event_skills WHERE event_seq IN (%s) ORDER BY skill ASC`, seqClause)
		skillRows, err := s.conn.QueryContext(ctx, query, seqArgs...)
		if err != nil {
			return nil, fmt.Errorf("query task skills: %w", err)
		}
		for skillRows.Next() {
			var eventSeq int64
			var skill string
			if err := skillRows.Scan(&eventSeq, &skill); err != nil {
				_ = skillRows.Close()
				return nil, fmt.Errorf("scan task skill: %w", err)
			}
			key := eventSeqToTask[eventSeq]
			t := get(key[0], key[1])
			t.Skills = append(t.Skills, skill)
		}
		if err := skillRows.Err(); err != nil {
			_ = skillRows.Close()
			return nil, fmt.Errorf("iterate task skills: %w", err)
		}
		_ = skillRows.Close()
	}

	out := make([]sqlTaskSummary, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, *t)
	}
	return out, nil
}

func int64InClause(values []int64) (string, []any) {
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return strings.Join(placeholders, ","), args
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
