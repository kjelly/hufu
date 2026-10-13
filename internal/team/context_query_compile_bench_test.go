package team

import (
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// Compiler and manifest costs exclude seed, retrieval, source hashing and
// fake-provider setup; the end-to-end benchmark covers those separately.
func BenchmarkContextQueryCompilerAndManifest(b *testing.B) {
	ctx, repo, scope, eligible := queryBenchmarkStore(b, 10, false)
	request := ContextRequest{SchemaVersion: 1, RunID: "benchmark", TaskID: "task", Attempt: 1, AgentName: "worker", AgentRole: "worker", ModelExecutionID: "execution", Goal: "inspect router", Constraints: "migration sentinel", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
	request.AssignRequestID()
	learning := agent.DefaultMemoryLearningPolicy()
	comparison, err := CompareContextQueryRanking(ctx, repo, repo, request, BuildContextQueryPlan(request, ContextQueryHints{}), scope, eligible, learning, defaultMemoryRuntimeRankingPolicy())
	if err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"legacy", "candidate"} {
		arm := comparison.Legacy
		if mode == "candidate" {
			arm = comparison.Candidate
		}
		input := WorkerContextInput{Request: request, Goal: request.Goal, CanonicalMemory: &CanonicalContextBundle{SharedPersistent: arm.Selected, SharedPersistentScores: arm.Scores, SharedPersistentFinalScores: arm.FinalScores, SharedPersistentAggregates: arm.Aggregates}, ModelContext: ModelContextSpec{ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32}}
		b.Run(mode, func(b *testing.B) {
			b.Run("compiler", func(b *testing.B) {
				var samples []float64
				b.ReportAllocs()
				for b.Loop() {
					started := time.Now()
					if _, err := CompileWorkerContext(ctx, input); err != nil {
						b.Fatal(err)
					}
					samples = append(samples, float64(time.Since(started).Nanoseconds())/1e6)
				}
				reportQueryBenchmark(b, samples, contextstore.QueryWork{})
			})
			b.Run("manifest", func(b *testing.B) {
				compiled, err := CompileWorkerContext(ctx, input)
				if err != nil {
					b.Fatal(err)
				}
				if mode == "candidate" {
					compiled.Query = &comparison.Selection.Attribution
				}
				var samples []float64
				b.ReportAllocs()
				for b.Loop() {
					started := time.Now()
					if _, err := BuildContextInjectionManifest(request, compiled, nil, "worker", contextstore.EvaluationTime(ctx), learning); err != nil {
						b.Fatal(err)
					}
					samples = append(samples, float64(time.Since(started).Nanoseconds())/1e6)
				}
				reportQueryBenchmark(b, samples, contextstore.QueryWork{})
			})
		})
	}
}
