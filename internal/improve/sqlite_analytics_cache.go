package improve

import (
	"context"
	"fmt"
)

// promptCacheTotals is the prompt-cache split for one selected run (or the
// sum over all selected runs). Input excludes cache reads, matching fantasy's
// openai-compatible usage mapping.
type promptCacheTotals struct {
	Input    int
	Read     int
	Creation int
}

func (t promptCacheTotals) add(other promptCacheTotals) promptCacheTotals {
	return promptCacheTotals{Input: t.Input + other.Input, Read: t.Read + other.Read, Creation: t.Creation + other.Creation}
}

// apply copies the cache counters into metrics. The hit rate is reads over
// every processed prompt token and is 0 when no prompt tokens were reported.
func (t promptCacheTotals) apply(metrics *Metrics) {
	metrics.PromptCacheReadTokens = t.Read
	metrics.PromptCacheCreationTokens = t.Creation
	metrics.PromptCacheHitRate = 0
	if total := t.Input + t.Read + t.Creation; total > 0 {
		metrics.PromptCacheHitRate = float64(t.Read) / float64(total)
	}
}

// promptCacheByOrdinalQuery uses the same event set as TotalTokens
// (tokensByAgentQuery): task events of a named team in the selected runs.
const promptCacheByOrdinalQuery = `
SELECT sr.ordinal,
       COALESCE(SUM(CASE WHEN e.input_tokens > 0 THEN e.input_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN e.cache_read_tokens > 0 THEN e.cache_read_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN e.cache_creation_tokens > 0 THEN e.cache_creation_tokens ELSE 0 END), 0)
FROM selected_runs sr
JOIN execution_events e ON e.run_id = sr.run_id
WHERE e.task_id <> '' AND e.team <> ''
GROUP BY sr.ordinal
ORDER BY sr.ordinal`

func (s *sqliteAnalyticsSession) sqlPromptCacheByOrdinal(ctx context.Context) (map[int]promptCacheTotals, error) {
	rows, err := s.executor.QueryContext(ctx, promptCacheByOrdinalQuery)
	if err != nil {
		return nil, fmt.Errorf("query prompt cache tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[int]promptCacheTotals{}
	for rows.Next() {
		var ordinal int
		var totals promptCacheTotals
		if err := rows.Scan(&ordinal, &totals.Input, &totals.Read, &totals.Creation); err != nil {
			return nil, fmt.Errorf("scan prompt cache tokens: %w", err)
		}
		result[ordinal] = totals
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompt cache tokens: %w", err)
	}
	return result, nil
}

func (s *sqliteAnalyticsSession) collectTrendPromptCache(ctx context.Context, trend []TrendPoint) error {
	byOrdinal, err := s.sqlPromptCacheByOrdinal(ctx)
	if err != nil {
		return err
	}
	for ordinal := range trend {
		byOrdinal[ordinal].apply(&trend[ordinal].Metrics)
	}
	return nil
}
