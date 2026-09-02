package improve

import (
	"context"
	"fmt"
)

// sqlCollectMemoryMetrics computes the memory metrics using the global
// TEMP memory_events scope. Memory events are intentionally not filtered by
// selected run or execution time: the canonical event-store reader always
// considered the complete canonical event store. Only the execution-derived
// token and retry denominators are scoped to runIDs.
func (s *sqliteAnalyticsSession) sqlCollectMemoryMetrics(ctx context.Context, runIDs []string, metrics *Metrics) error {
	if metrics == nil {
		return fmt.Errorf("collect memory metrics: nil metrics")
	}

	const memorySummaryQuery = `
SELECT
    COUNT(DISTINCT CASE WHEN retrieval_id <> '' THEN retrieval_id END),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_usage_recorded' AND disposition = 'applied' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' AND reason_code = 'stale_environment' THEN 1 ELSE 0 END), 0)
      + COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND signal = 'stale_environment' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND signal = 'verification_passed' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND direction = 'negative' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' AND token_count > 0 THEN token_count ELSE 0 END), 0)
FROM memory_events`
	var retrievalCount, exposureCount, appliedCount, staleCount, verifiedCount, harmfulCount, memoryTokens int
	if err := s.conn.QueryRowContext(ctx, memorySummaryQuery).Scan(
		&retrievalCount, &exposureCount, &appliedCount, &staleCount,
		&verifiedCount, &harmfulCount, &memoryTokens,
	); err != nil {
		return fmt.Errorf("query memory metrics summary: %w", err)
	}

	inputTokens, err := s.sqlSelectedInputTokens(ctx, runIDs)
	if err != nil {
		return err
	}
	assistedRetries, unassistedRetries, appliedTaskCount, err := s.sqlMemoryRetryCounts(ctx, runIDs)
	if err != nil {
		return err
	}

	metrics.MemoryRetrievalCount = retrievalCount
	metrics.MemoryExposureCount = exposureCount
	metrics.MemoryAppliedCount = appliedCount
	if metrics.MemoryExposureCount > 0 {
		metrics.MemoryAttributionCoverage = float64(metrics.MemoryAppliedCount) / float64(metrics.MemoryExposureCount)
		metrics.MemoryStaleRetrievalRate = float64(staleCount) / float64(metrics.MemoryExposureCount)
	}
	if metrics.MemoryAppliedCount > 0 {
		metrics.MemoryVerifiedAssistRate = float64(verifiedCount) / float64(metrics.MemoryAppliedCount)
		metrics.MemoryHarmfulUseRate = float64(harmfulCount) / float64(metrics.MemoryAppliedCount)
	}
	if inputTokens > 0 {
		metrics.MemoryTokenOverhead = float64(memoryTokens) / float64(inputTokens)
	}
	if appliedTaskCount > 0 {
		metrics.MemoryAssistedRetryRate = float64(assistedRetries) / float64(appliedTaskCount)
	}
	unassistedTotal := metrics.TotalTasks - appliedTaskCount
	if unassistedTotal > 0 {
		metrics.MemoryUnassistedRetryRate = float64(unassistedRetries) / float64(unassistedTotal)
	}
	return nil
}

func (s *sqliteAnalyticsSession) sqlSelectedInputTokens(ctx context.Context, runIDs []string) (int, error) {
	if len(runIDs) == 0 {
		return 0, nil
	}
	inClause, args := runsInClause(runIDs)
	query := fmt.Sprintf(`
SELECT COALESCE(SUM(CASE WHEN input_tokens > 0 THEN input_tokens ELSE 0 END), 0)
FROM execution_events
WHERE team <> '' AND run_id IN (%s)`, inClause)
	var inputTokens int
	if err := s.conn.QueryRowContext(ctx, query, args...).Scan(&inputTokens); err != nil {
		return 0, fmt.Errorf("query selected memory input tokens: %w", err)
	}
	return inputTokens, nil
}

func (s *sqliteAnalyticsSession) sqlMemoryRetryCounts(ctx context.Context, runIDs []string) (assisted, unassisted, appliedTasks int, err error) {
	const appliedTasksQuery = `
SELECT COUNT(*)
FROM (
    SELECT run_id, task_id
    FROM memory_events
    WHERE type = 'memory_usage_recorded' AND disposition = 'applied'
    GROUP BY run_id, task_id
)`
	if err := s.conn.QueryRowContext(ctx, appliedTasksQuery).Scan(&appliedTasks); err != nil {
		return 0, 0, 0, fmt.Errorf("query applied memory tasks: %w", err)
	}
	if len(runIDs) == 0 {
		return 0, 0, appliedTasks, nil
	}

	inClause, args := runsInClause(runIDs)
	query := fmt.Sprintf(`
WITH applied AS (
    SELECT run_id, task_id
    FROM memory_events
    WHERE type = 'memory_usage_recorded' AND disposition = 'applied'
    GROUP BY run_id, task_id
), selected_retries AS (
    SELECT run_id, task_id
    FROM execution_events
    WHERE team <> '' AND task_id <> '' AND attempt > 1 AND run_id IN (%s)
    GROUP BY run_id, task_id
)
SELECT
    COALESCE(SUM(CASE WHEN applied.run_id IS NOT NULL THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN applied.run_id IS NULL THEN 1 ELSE 0 END), 0)
FROM selected_retries
LEFT JOIN applied ON applied.run_id = selected_retries.run_id
                  AND applied.task_id = selected_retries.task_id`, inClause)
	if err := s.conn.QueryRowContext(ctx, query, args...).Scan(&assisted, &unassisted); err != nil {
		return 0, 0, 0, fmt.Errorf("query memory retry counts: %w", err)
	}
	return assisted, unassisted, appliedTasks, nil
}
