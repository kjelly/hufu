package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

func TestWriteExecutionRouteReport(t *testing.T) {
	primary := execution.ExecutionTarget{Backend: "ollama", Model: "qwen3"}
	fallback := execution.ExecutionTarget{Backend: "openai", Model: "gpt-5"}
	routed := &team.TodoItem{
		ID: "3", ExecutionTarget: primary,
		ExecutionRoute: &team.ExecutionRouteBinding{Name: "coding", Candidates: []execution.ExecutionTarget{primary, fallback}},
		ExecutionReceipts: []team.ExecutionReceipt{
			{Attempt: 1, ExecutionTarget: primary},
			{Attempt: 2, ExecutionTarget: fallback, FallbackFrom: &primary, FallbackFailureClass: team.ProviderRateLimited},
		},
	}
	var b strings.Builder
	writeExecutionRouteReport(&b, []*team.TodoItem{{ID: "1"}, routed}, &team.RunMetrics{
		WorkerFallbacksTotal: 1, WorkerFallbacksByClass: map[team.ProviderFailureClass]int{team.ProviderRateLimited: 1},
	})
	report := b.String()
	for _, want := range []string{
		"### Execution Routes", "**Fallbacks:** 1 (rate_limited: 1)",
		"| 3 | coding | ollama/qwen3 → openai/gpt-5 | #1 ollama/qwen3; #2 openai/gpt-5 (fallback after rate_limited) |",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	var empty strings.Builder
	writeExecutionRouteReport(&empty, []*team.TodoItem{{ID: "1"}}, nil)
	if empty.Len() != 0 {
		t.Fatalf("a run without routes rendered %q", empty.String())
	}
}
