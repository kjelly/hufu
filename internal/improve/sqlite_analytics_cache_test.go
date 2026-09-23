package improve

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestPromptCacheTotalsApply(t *testing.T) {
	cases := []struct {
		name   string
		totals promptCacheTotals
		want   float64
	}{
		{name: "no prompt tokens", totals: promptCacheTotals{}, want: 0},
		{name: "no cache", totals: promptCacheTotals{Input: 100}, want: 0},
		{name: "reads only", totals: promptCacheTotals{Input: 25, Read: 75}, want: 0.75},
		{name: "reads and writes", totals: promptCacheTotals{Input: 20, Read: 60, Creation: 20}, want: 0.6},
		{name: "fully cached", totals: promptCacheTotals{Read: 10}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := Metrics{PromptCacheHitRate: 42}
			tc.totals.apply(&metrics)
			if metrics.PromptCacheReadTokens != tc.totals.Read || metrics.PromptCacheCreationTokens != tc.totals.Creation {
				t.Fatalf("counters = %d/%d, want %d/%d", metrics.PromptCacheReadTokens, metrics.PromptCacheCreationTokens, tc.totals.Read, tc.totals.Creation)
			}
			if math.Abs(metrics.PromptCacheHitRate-tc.want) > 1e-9 {
				t.Fatalf("hit rate = %v, want %v", metrics.PromptCacheHitRate, tc.want)
			}
		})
	}
}

func writeCacheAnalyticsTeam(t *testing.T) string {
	t.Helper()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\n---\nFix bugs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return teamDir
}

func TestAnalyzeRecentPromptCacheMetricsMatchTrend(t *testing.T) {
	workspace := t.TempDir()
	teamDir := writeCacheAnalyticsTeam(t)
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{InputTokens: 40, OutputTokens: 5, TotalTokens: 105, CacheReadTokens: 60}},
		// A run-level event (empty task ID) is outside the TotalTokens event
		// set and must not change the hit rate.
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "run-1", Team: "dev", Status: "done", Usage: team.ExecutionUsage{InputTokens: 1000}},
		{Timestamp: "2026-07-12T11:00:00Z", RunID: "run-2", Team: "dev", TaskID: "task-2", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{InputTokens: 50, TotalTokens: 100, CacheCreationTokens: 50}},
	})

	report, err := AnalyzeRecent(workspace, "dev", teamDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.PromptCacheReadTokens != 60 || report.Metrics.PromptCacheCreationTokens != 50 {
		t.Fatalf("cache counters = %+v", report.Metrics)
	}
	if want := 60.0 / 200.0; math.Abs(report.Metrics.PromptCacheHitRate-want) > 1e-9 {
		t.Fatalf("hit rate = %v, want %v", report.Metrics.PromptCacheHitRate, want)
	}
	if len(report.Trend) != 2 {
		t.Fatalf("trend = %+v", report.Trend)
	}
	var reads, creations int
	for _, point := range report.Trend {
		reads += point.Metrics.PromptCacheReadTokens
		creations += point.Metrics.PromptCacheCreationTokens
		switch point.RunID {
		case "run-1":
			if math.Abs(point.Metrics.PromptCacheHitRate-0.6) > 1e-9 {
				t.Fatalf("run-1 hit rate = %v", point.Metrics.PromptCacheHitRate)
			}
		case "run-2":
			if point.Metrics.PromptCacheHitRate != 0 {
				t.Fatalf("run-2 hit rate = %v", point.Metrics.PromptCacheHitRate)
			}
		}
	}
	if reads != report.Metrics.PromptCacheReadTokens || creations != report.Metrics.PromptCacheCreationTokens {
		t.Fatalf("trend cache sums %d/%d differ from report %+v", reads, creations, report.Metrics)
	}
}

func TestMemoryTokenOverheadUsesPromptTokens(t *testing.T) {
	workspace := t.TempDir()
	teamDir := writeCacheAnalyticsTeam(t)
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{InputTokens: 10, CacheReadTokens: 30, CacheCreationTokens: 10}},
	})
	writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "retrieval-1", RunID: "run-1", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1","context_item_id":"context-1","content_hash":"hash-1","policy_version":"policy-v1","token_count":5}`)},
	})

	report, err := AnalyzeRecent(workspace, "dev", teamDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	// 5 memory tokens over 10 uncached + 30 read + 10 written prompt tokens.
	if want := 5.0 / 50.0; math.Abs(report.Metrics.MemoryTokenOverhead-want) > 1e-9 {
		t.Fatalf("memory token overhead = %v, want %v", report.Metrics.MemoryTokenOverhead, want)
	}
}
