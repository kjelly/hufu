package improve

import (
	"context"
	"fmt"
	"time"
)

// sqlCollectTrend builds every selected run's metrics with a constant number
// of set-based query families. The returned order is selected_runs.ordinal.
func (s *sqliteAnalyticsSession) sqlCollectTrend(
	ctx context.Context,
	memory memoryAnalytics,
	revisions map[int]string,
) ([]TrendPoint, error) {
	selected, err := s.sqlSelectedRuns(ctx)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	trend := make([]TrendPoint, len(selected))
	for _, run := range selected {
		trend[run.Ordinal] = TrendPoint{
			RunID: run.RunID, StartedAt: unixNSToTime(run.StartUnixNS).Format(time.RFC3339),
			EndedAt: unixNSToTime(run.EndUnixNS).Format(time.RFC3339), TeamRevision: revisions[run.Ordinal],
			Metrics: Metrics{
				RunID: run.RunID, RunCount: 1,
				StartedAt: unixNSToTime(run.StartUnixNS).Format(time.RFC3339), EndedAt: unixNSToTime(run.EndUnixNS).Format(time.RFC3339),
				TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{},
			},
		}
	}
	if err := s.collectTrendTasks(ctx, trend); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	if err := s.collectTrendTokens(ctx, trend); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	if err := s.collectTrendPromptCache(ctx, trend); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	if err := s.collectTrendAudit(ctx, trend); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	for ordinal := range trend {
		if err := memory.apply(&trend[ordinal].Metrics, ordinal); err != nil {
			return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
		}
	}
	return trend, nil
}

func (s *sqliteAnalyticsSession) collectTrendTasks(ctx context.Context, trend []TrendPoint) error {
	const query = `
SELECT sr.ordinal,
       COUNT(t.task_id),
       COALESCE(SUM(CASE WHEN t.terminal = 'done' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN t.terminal = 'error' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN t.terminal = 'planned' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(t.total_attempts), 0),
       COALESCE(SUM(CASE WHEN t.attempts > 1 THEN 1 ELSE 0 END), 0)
FROM selected_runs sr
LEFT JOIN task_summary t ON t.run_id = sr.run_id
GROUP BY sr.ordinal
ORDER BY sr.ordinal`
	rows, err := s.executor.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query trend task metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ordinal int
		var metrics Metrics
		if err := rows.Scan(&ordinal, &metrics.TotalTasks, &metrics.Done, &metrics.Error, &metrics.Planned, &metrics.TotalAttempts, &metrics.RetriedTasks); err != nil {
			return fmt.Errorf("scan trend task metrics: %w", err)
		}
		if ordinal < 0 || ordinal >= len(trend) {
			return fmt.Errorf("scan trend task metrics: ordinal %d outside selected scope", ordinal)
		}
		trend[ordinal].Metrics.TotalTasks = metrics.TotalTasks
		trend[ordinal].Metrics.Done = metrics.Done
		trend[ordinal].Metrics.Error = metrics.Error
		trend[ordinal].Metrics.Planned = metrics.Planned
		trend[ordinal].Metrics.TotalAttempts = metrics.TotalAttempts
		trend[ordinal].Metrics.RetriedTasks = metrics.RetriedTasks
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate trend task metrics: %w", err)
	}
	return nil
}

func (s *sqliteAnalyticsSession) collectTrendTokens(ctx context.Context, trend []TrendPoint) error {
	const query = `
SELECT sr.ordinal,
       CASE WHEN e.agent = '' THEN 'unspecified' ELSE e.agent END AS agent_key,
       SUM(e.total_tokens)
FROM selected_runs sr
JOIN execution_events e ON e.run_id = sr.run_id
WHERE e.task_id <> '' AND e.team <> ''
GROUP BY sr.ordinal, agent_key
ORDER BY sr.ordinal, agent_key`
	rows, err := s.executor.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query trend tokens by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ordinal, tokens int
		var agent string
		if err := rows.Scan(&ordinal, &agent, &tokens); err != nil {
			return fmt.Errorf("scan trend tokens by agent: %w", err)
		}
		if ordinal < 0 || ordinal >= len(trend) {
			return fmt.Errorf("scan trend tokens by agent: ordinal %d outside selected scope", ordinal)
		}
		trend[ordinal].Metrics.TokensByAgent[agent] = tokens
		trend[ordinal].Metrics.TotalTokens += tokens
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate trend tokens by agent: %w", err)
	}
	return nil
}

func (s *sqliteAnalyticsSession) collectTrendAudit(ctx context.Context, trend []TrendPoint) error {
	const query = `
SELECT sr.ordinal, COALESCE(a.agent, '') AS agent,
       COALESCE(SUM(CASE WHEN a.event = 'tool_call' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN a.event = 'tool_error' THEN 1 ELSE 0 END), 0)
FROM selected_runs sr
LEFT JOIN audit_events a
  ON a.team = sr.team
 AND a.timestamp_unix_ns >= sr.start_ns
 AND a.timestamp_unix_ns <= sr.end_ns
GROUP BY sr.ordinal, COALESCE(a.agent, '')
ORDER BY sr.ordinal, agent`
	rows, err := s.executor.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query trend audit metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ordinal, calls, errors int
		var agent string
		if err := rows.Scan(&ordinal, &agent, &calls, &errors); err != nil {
			return fmt.Errorf("scan trend audit metrics: %w", err)
		}
		if ordinal < 0 || ordinal >= len(trend) {
			return fmt.Errorf("scan trend audit metrics: ordinal %d outside selected scope", ordinal)
		}
		trend[ordinal].Metrics.ToolCalls += calls
		trend[ordinal].Metrics.ToolErrors += errors
		if calls > 0 {
			trend[ordinal].Metrics.ToolCallsByAgent[agent] = calls
		}
		if errors > 0 {
			trend[ordinal].Metrics.ToolErrorsByAgent[agent] = errors
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate trend audit metrics: %w", err)
	}
	return nil
}
