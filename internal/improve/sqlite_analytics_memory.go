package improve

import (
	"context"
	"fmt"
)

func (s *sqliteAnalyticsSession) sqlMemoryEvidence(ctx context.Context) ([]string, []ArtifactRef, error) {
	const policyQuery = `
SELECT DISTINCT m.policy_version
FROM memory_events m
JOIN selected_runs sr ON sr.run_id = m.run_id
WHERE m.policy_version <> ''
ORDER BY m.policy_version`
	policies, err := queryStrings(ctx, s, policyQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("query selected memory policy versions: %w", err)
	}
	const contextQuery = `
SELECT DISTINCT m.context_item_id, m.content_hash
FROM memory_events m
JOIN selected_runs sr ON sr.run_id = m.run_id
WHERE m.type = 'memory_usage_recorded'
	  AND m.disposition = 'applied' AND m.context_item_id <> '' AND m.content_hash <> ''
ORDER BY m.context_item_id, m.content_hash`
	rows, err := s.conn.QueryContext(ctx, contextQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("query selected applied context items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []ArtifactRef
	for rows.Next() {
		var id, revision string
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, nil, fmt.Errorf("scan selected applied context item: %w", err)
		}
		refs = append(refs, ArtifactRef{Kind: "context_item", ID: id, Revision: revision})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate selected applied context items: %w", err)
	}
	return policies, refs, nil
}

func queryStrings(ctx context.Context, s *sqliteAnalyticsSession, query string, args ...any) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// sqlCollectMemoryMetrics computes the memory metrics using the global
// TEMP memory_events scope. Memory events are intentionally not filtered by
// selected run or execution time: the canonical event-store reader always
// considered the complete canonical event store. Only the execution-derived
// token and retry denominators are scoped to selected_runs.
func (s *sqliteAnalyticsSession) sqlCollectMemoryMetrics(ctx context.Context, ordinal int, metrics *Metrics) error {
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

	inputTokens, err := s.sqlSelectedInputTokens(ctx, ordinal)
	if err != nil {
		return err
	}
	assistedRetries, unassistedRetries, appliedTaskCount, err := s.sqlMemoryRetryCounts(ctx, ordinal)
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

func (s *sqliteAnalyticsSession) sqlSelectedInputTokens(ctx context.Context, ordinal int) (int, error) {
	const query = `
SELECT COALESCE(SUM(CASE WHEN e.input_tokens > 0 THEN e.input_tokens ELSE 0 END), 0)
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
WHERE e.team <> '' AND (? < 0 OR sr.ordinal = ?)`
	var inputTokens int
	if err := s.conn.QueryRowContext(ctx, query, ordinal, ordinal).Scan(&inputTokens); err != nil {
		return 0, fmt.Errorf("query selected memory input tokens: %w", err)
	}
	return inputTokens, nil
}

func (s *sqliteAnalyticsSession) sqlMemoryRetryCounts(ctx context.Context, ordinal int) (assisted, unassisted, appliedTasks int, err error) {
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
	const query = `
WITH applied AS (
    SELECT run_id, task_id
    FROM memory_events
    WHERE type = 'memory_usage_recorded' AND disposition = 'applied'
    GROUP BY run_id, task_id
), selected_retries AS (
    SELECT e.run_id, e.task_id
    FROM execution_events e
    JOIN selected_runs sr ON sr.run_id = e.run_id
    WHERE e.team <> '' AND e.task_id <> '' AND e.attempt > 1
      AND (? < 0 OR sr.ordinal = ?)
    GROUP BY e.run_id, e.task_id
)
SELECT
    COALESCE(SUM(CASE WHEN applied.run_id IS NOT NULL THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN applied.run_id IS NULL THEN 1 ELSE 0 END), 0)
FROM selected_retries
LEFT JOIN applied ON applied.run_id = selected_retries.run_id
                  AND applied.task_id = selected_retries.task_id`
	if err := s.conn.QueryRowContext(ctx, query, ordinal, ordinal).Scan(&assisted, &unassisted); err != nil {
		return 0, 0, 0, fmt.Errorf("query memory retry counts: %w", err)
	}
	return assisted, unassisted, appliedTasks, nil
}
