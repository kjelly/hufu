package improve

// SQL grouped aggregation (spec.md §10.2), built on the
// task_summary/task_skills TEMP tables materialized by
// sqlite_analytics_task_summary.go. One parameterized query template
// handles all three "single value per task" dimensions (agent/model/
// task_type); skill gets its own query because a task can belong to more
// than one skill group (spec.md's documented BySkill overlap).
//
// The dimension's SQL column name is never taken from caller-supplied
// input — groupDimensionColumns is a closed, package-internal allow-list,
// per spec.md §10.2's "不要使用字串拼接直接把任意 user input 當 column
// name" rule.

import (
	"context"
	"fmt"
)

// groupDimensionColumns is the closed set of task_summary columns
// SQL grouping is allowed to group by, each paired with the same fallback
// label required for that dimension.
var groupDimensionColumns = map[string]string{
	"agent":     "unspecified",
	"model":     "unspecified",
	"task_type": "legacy/unspecified",
}

const groupByDimensionQueryTemplate = `
SELECT CASE WHEN TRIM(%[1]s, char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
                                  8192, 8193, 8194, 8195, 8196, 8197, 8198,
                                  8199, 8200, 8201, 8202, 8232, 8233, 8239,
                                  8287, 12288)) = '' THEN ? ELSE %[1]s END AS group_key,
       COUNT(*) AS total_tasks,
       SUM(CASE WHEN terminal = 'done' THEN 1 ELSE 0 END) AS done,
       SUM(CASE WHEN terminal = 'error' THEN 1 ELSE 0 END) AS error,
       SUM(CASE WHEN terminal = 'planned' THEN 1 ELSE 0 END) AS planned,
       SUM(total_attempts) AS total_attempts,
       SUM(CASE WHEN attempts > 1 THEN 1 ELSE 0 END) AS retried_tasks,
       SUM(total_tokens) AS total_tokens
FROM task_summary
WHERE run_id IN (%[2]s)
GROUP BY group_key
ORDER BY group_key ASC`

// groupBySkillQuery LEFT JOINs task_skills so a task with zero skills
// produces exactly one 'none' row and a task with N skills produces N rows
// (one per skill group), preserving the BySkill
// overlap-by-design behavior in one query instead of a separate "tasks
// without any skill" branch.
const groupBySkillQuery = `
SELECT COALESCE(ts.skill, 'none') AS group_key,
       COUNT(*) AS total_tasks,
       SUM(CASE WHEN t.terminal = 'done' THEN 1 ELSE 0 END) AS done,
       SUM(CASE WHEN t.terminal = 'error' THEN 1 ELSE 0 END) AS error,
       SUM(CASE WHEN t.terminal = 'planned' THEN 1 ELSE 0 END) AS planned,
       SUM(t.total_attempts) AS total_attempts,
       SUM(CASE WHEN t.attempts > 1 THEN 1 ELSE 0 END) AS retried_tasks,
       SUM(t.total_tokens) AS total_tokens
FROM task_summary t
LEFT JOIN task_skills ts ON ts.run_id = t.run_id AND ts.task_id = t.task_id
WHERE t.run_id IN (%s)
GROUP BY group_key
ORDER BY group_key ASC`

func (s *sqliteAnalyticsSession) sqlGroupMetricsByDimension(ctx context.Context, runIDs []string, dimension string) ([]GroupMetric, error) {
	fallback, ok := groupDimensionColumns[dimension]
	if !ok {
		return nil, fmt.Errorf("unknown group dimension %q", dimension)
	}
	if len(runIDs) == 0 {
		return []GroupMetric{}, nil
	}
	if err := s.ensureTaskViews(ctx); err != nil {
		return nil, err
	}
	inClause, runArgs := runsInClause(runIDs)
	query := fmt.Sprintf(groupByDimensionQueryTemplate, dimension, inClause)
	args := append([]any{fallback}, runArgs...)
	return s.scanGroupMetrics(ctx, query, args)
}

func (s *sqliteAnalyticsSession) sqlGroupMetricsBySkill(ctx context.Context, runIDs []string) ([]GroupMetric, error) {
	if len(runIDs) == 0 {
		return []GroupMetric{}, nil
	}
	if err := s.ensureTaskViews(ctx); err != nil {
		return nil, err
	}
	inClause, args := runsInClause(runIDs)
	return s.scanGroupMetrics(ctx, fmt.Sprintf(groupBySkillQuery, inClause), args)
}

func (s *sqliteAnalyticsSession) scanGroupMetrics(ctx context.Context, query string, args []any) ([]GroupMetric, error) {
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query grouped metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// The public grouped fields are always non-nil, possibly-empty
	// slice (make([]GroupMetric, 0, len(groups))); match that exactly so a
	// zero-task run scope remains a stable empty public collection.
	out := make([]GroupMetric, 0)
	for rows.Next() {
		var g GroupMetric
		if err := rows.Scan(&g.Key, &g.TotalTasks, &g.Done, &g.Error, &g.Planned, &g.TotalAttempts, &g.RetriedTasks, &g.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan grouped metrics: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate grouped metrics: %w", err)
	}
	return out, nil
}

// sqlCollectGroupedMetrics performs grouped SQL aggregation,
// scoped to the given run IDs.
func (s *sqliteAnalyticsSession) sqlCollectGroupedMetrics(ctx context.Context, runIDs []string) (GroupedMetrics, error) {
	byAgent, err := s.sqlGroupMetricsByDimension(ctx, runIDs, "agent")
	if err != nil {
		return GroupedMetrics{}, err
	}
	byTaskType, err := s.sqlGroupMetricsByDimension(ctx, runIDs, "task_type")
	if err != nil {
		return GroupedMetrics{}, err
	}
	byModel, err := s.sqlGroupMetricsByDimension(ctx, runIDs, "model")
	if err != nil {
		return GroupedMetrics{}, err
	}
	bySkill, err := s.sqlGroupMetricsBySkill(ctx, runIDs)
	if err != nil {
		return GroupedMetrics{}, err
	}
	return GroupedMetrics{ByAgent: byAgent, ByTaskType: byTaskType, ByModel: byModel, BySkill: bySkill}, nil
}
