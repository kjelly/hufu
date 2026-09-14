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
	rows, err := s.executor.QueryContext(ctx, contextQuery)
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
	rows, err := s.executor.QueryContext(ctx, query, args...)
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

const allSelectedRunOrdinals = -1

type memoryGlobalMetrics struct {
	RetrievalCount int
	ExposureCount  int
	AppliedCount   int
	StaleCount     int
	VerifiedCount  int
	HarmfulCount   int
	MemoryTokens   int
	AppliedTasks   int
}

type memoryScopeMetrics struct {
	InputTokens       int
	AssistedRetries   int
	UnassistedRetries int
}

type memoryAnalytics struct {
	Global    memoryGlobalMetrics
	Overall   memoryScopeMetrics
	ByOrdinal map[int]memoryScopeMetrics
}

// sqlCollectMemoryAnalytics computes global canonical-memory counters once,
// then computes every selected-run denominator in one set-based query.
func (s *sqliteAnalyticsSession) sqlCollectMemoryAnalytics(ctx context.Context) (memoryAnalytics, error) {
	const globalQuery = `
SELECT
    COUNT(DISTINCT CASE WHEN retrieval_id <> '' THEN retrieval_id END),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_usage_recorded' AND disposition = 'applied' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' AND reason_code = 'stale_environment' THEN 1 ELSE 0 END), 0)
      + COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND signal = 'stale_environment' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND signal = 'verification_passed' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_outcome_recorded' AND direction = 'negative' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'memory_retrieved' AND token_count > 0 THEN token_count ELSE 0 END), 0),
    (SELECT COUNT(*) FROM (
        SELECT run_id, task_id
        FROM memory_events
        WHERE type = 'memory_usage_recorded' AND disposition = 'applied'
        GROUP BY run_id, task_id
    ))
FROM memory_events`
	var result memoryAnalytics
	result.ByOrdinal = make(map[int]memoryScopeMetrics)
	if err := s.executor.QueryRowContext(ctx, globalQuery).Scan(
		&result.Global.RetrievalCount, &result.Global.ExposureCount, &result.Global.AppliedCount,
		&result.Global.StaleCount, &result.Global.VerifiedCount, &result.Global.HarmfulCount,
		&result.Global.MemoryTokens, &result.Global.AppliedTasks,
	); err != nil {
		return memoryAnalytics{}, fmt.Errorf("query global memory metrics: %w", err)
	}
	const scopeQuery = `
WITH input_by_run AS (
    SELECT sr.ordinal,
           COALESCE(SUM(CASE WHEN e.input_tokens > 0 THEN e.input_tokens ELSE 0 END), 0) AS input_tokens
    FROM selected_runs sr
    LEFT JOIN execution_events e ON e.run_id = sr.run_id AND e.team <> ''
    GROUP BY sr.ordinal
), applied AS (
    SELECT run_id, task_id
    FROM memory_events
    WHERE type = 'memory_usage_recorded' AND disposition = 'applied'
    GROUP BY run_id, task_id
), retry_pairs AS (
    SELECT e.run_id, e.task_id
    FROM execution_events e
    JOIN selected_runs sr ON sr.run_id = e.run_id
    WHERE e.team <> '' AND e.task_id <> '' AND e.attempt > 1
    GROUP BY e.run_id, e.task_id
), retry_by_run AS (
    SELECT sr.ordinal,
           COALESCE(SUM(CASE WHEN rp.run_id IS NOT NULL AND a.run_id IS NOT NULL THEN 1 ELSE 0 END), 0) AS assisted,
           COALESCE(SUM(CASE WHEN rp.run_id IS NOT NULL AND a.run_id IS NULL THEN 1 ELSE 0 END), 0) AS unassisted
    FROM selected_runs sr
    LEFT JOIN retry_pairs rp ON rp.run_id = sr.run_id
    LEFT JOIN applied a ON a.run_id = rp.run_id AND a.task_id = rp.task_id
    GROUP BY sr.ordinal
)
SELECT sr.ordinal, COALESCE(i.input_tokens, 0), COALESCE(r.assisted, 0), COALESCE(r.unassisted, 0)
FROM selected_runs sr
LEFT JOIN input_by_run i ON i.ordinal = sr.ordinal
LEFT JOIN retry_by_run r ON r.ordinal = sr.ordinal
ORDER BY sr.ordinal`
	rows, err := s.executor.QueryContext(ctx, scopeQuery)
	if err != nil {
		return memoryAnalytics{}, fmt.Errorf("query selected memory denominators: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ordinal int
		var scope memoryScopeMetrics
		if err := rows.Scan(&ordinal, &scope.InputTokens, &scope.AssistedRetries, &scope.UnassistedRetries); err != nil {
			return memoryAnalytics{}, fmt.Errorf("scan selected memory denominators: %w", err)
		}
		result.ByOrdinal[ordinal] = scope
		result.Overall.InputTokens += scope.InputTokens
		result.Overall.AssistedRetries += scope.AssistedRetries
		result.Overall.UnassistedRetries += scope.UnassistedRetries
	}
	if err := rows.Err(); err != nil {
		return memoryAnalytics{}, fmt.Errorf("iterate selected memory denominators: %w", err)
	}
	return result, nil
}

func (analytics memoryAnalytics) apply(metrics *Metrics, ordinal int) error {
	if metrics == nil {
		return fmt.Errorf("apply memory metrics: nil metrics")
	}
	scope := analytics.Overall
	if ordinal >= 0 {
		scope = analytics.ByOrdinal[ordinal]
	}
	metrics.MemoryRetrievalCount = analytics.Global.RetrievalCount
	metrics.MemoryExposureCount = analytics.Global.ExposureCount
	metrics.MemoryAppliedCount = analytics.Global.AppliedCount
	if metrics.MemoryExposureCount > 0 {
		metrics.MemoryAttributionCoverage = float64(metrics.MemoryAppliedCount) / float64(metrics.MemoryExposureCount)
		metrics.MemoryStaleRetrievalRate = float64(analytics.Global.StaleCount) / float64(metrics.MemoryExposureCount)
	}
	if metrics.MemoryAppliedCount > 0 {
		metrics.MemoryVerifiedAssistRate = float64(analytics.Global.VerifiedCount) / float64(metrics.MemoryAppliedCount)
		metrics.MemoryHarmfulUseRate = float64(analytics.Global.HarmfulCount) / float64(metrics.MemoryAppliedCount)
	}
	if scope.InputTokens > 0 {
		metrics.MemoryTokenOverhead = float64(analytics.Global.MemoryTokens) / float64(scope.InputTokens)
	}
	if analytics.Global.AppliedTasks > 0 {
		metrics.MemoryAssistedRetryRate = float64(scope.AssistedRetries) / float64(analytics.Global.AppliedTasks)
	}
	unassistedTotal := metrics.TotalTasks - analytics.Global.AppliedTasks
	if unassistedTotal > 0 {
		metrics.MemoryUnassistedRetryRate = float64(scope.UnassistedRetries) / float64(unassistedTotal)
	}
	return nil
}
