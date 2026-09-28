package evalharness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPerformanceSamplesStrictlyValidatesArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.json")
	if err := os.WriteFile(path, []byte(`[{"task_class":"simple-query","duration_ns":1,"accepted":true,"total_tokens":2,"provider_calls":1,"retries":0}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	samples, err := LoadPerformanceSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].TaskClass != TaskClassSimpleQuery {
		t.Fatalf("samples = %#v", samples)
	}

	if err := os.WriteFile(path, []byte(`[{"task_class":"typo","duration_ns":1,"accepted":true,"total_tokens":2,"provider_calls":1,"retries":0,"extra":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPerformanceSamples(path); err == nil {
		t.Fatal("artifact with unknown fields and task class was accepted")
	}
}

func TestEvaluatePerformanceGatePassesNonRegressingCandidate(t *testing.T) {
	baseline := performanceSamples(100*time.Millisecond, 100, 2, 1, true)
	candidate := performanceSamples(90*time.Millisecond, 90, 2, 1, true)

	report, err := EvaluatePerformanceGate(baseline, candidate, DefaultPerformanceGateBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || len(report.Findings) != 0 {
		t.Fatalf("gate should pass: %#v", report.Findings)
	}
	for _, taskClass := range RequiredPerformanceTaskClasses {
		summary := report.Candidate[taskClass]
		if summary.P50Latency != 90*time.Millisecond || summary.P95Latency != 90*time.Millisecond {
			t.Fatalf("%s latency summary = %#v", taskClass, summary)
		}
	}
}

func TestEvaluatePerformanceGateBlocksCorrectnessAndEfficiencyRegressions(t *testing.T) {
	baseline := performanceSamples(100*time.Millisecond, 100, 2, 0, true)
	candidate := performanceSamples(200*time.Millisecond, 150, 3, 1, true)
	// Correctness is measured over every invocation, not only accepted latency.
	candidate = append(candidate, PerformanceSample{TaskClass: TaskClassVerification, Duration: time.Millisecond, Accepted: false})

	report, err := EvaluatePerformanceGate(baseline, candidate, DefaultPerformanceGateBudget())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("gate passed despite correctness and efficiency regressions")
	}
	wantMetrics := map[string]bool{
		"acceptance-rate":    false,
		"p95-latency":        false,
		"p95-total-tokens":   false,
		"p95-provider-calls": false,
		"p95-retries":        false,
	}
	for _, finding := range report.Findings {
		if _, ok := wantMetrics[finding.Metric]; ok {
			wantMetrics[finding.Metric] = true
		}
	}
	for metric, found := range wantMetrics {
		if !found {
			t.Errorf("missing %s regression finding: %#v", metric, report.Findings)
		}
	}
}

func TestEvaluatePerformanceGateRequiresAllTaskClasses(t *testing.T) {
	baseline := performanceSamples(time.Millisecond, 1, 1, 0, true)
	candidate := performanceSamples(time.Millisecond, 1, 1, 0, true)
	candidate = candidate[:len(candidate)-1]

	report, err := EvaluatePerformanceGate(baseline, candidate, DefaultPerformanceGateBudget())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || len(report.Findings) != 1 {
		t.Fatalf("missing task class findings = %#v", report.Findings)
	}
	if report.Findings[0].TaskClass != TaskClassFailureRecovery || report.Findings[0].Metric != "candidate-samples" {
		t.Fatalf("unexpected finding: %#v", report.Findings[0])
	}
}

func TestEvaluatePerformanceGateRejectsInvalidBudget(t *testing.T) {
	_, err := EvaluatePerformanceGate(nil, nil, PerformanceGateBudget{
		MaxP95LatencyRatio:       0.9,
		MaxP95TokensRatio:        1,
		MaxP95ProviderCallsRatio: 1,
		MaxP95RetriesRatio:       1,
		MinSamplesPerClass:       1,
	})
	if err == nil {
		t.Fatal("invalid ratio was accepted")
	}
}

func TestPerformanceSummaryUsesAcceptedSamplesAndNearestRank(t *testing.T) {
	samples := []PerformanceSample{
		{TaskClass: TaskClassSimpleQuery, Duration: 10 * time.Millisecond, Accepted: true, TotalTokens: 10},
		{TaskClass: TaskClassSimpleQuery, Duration: 20 * time.Millisecond, Accepted: true, TotalTokens: 20},
		{TaskClass: TaskClassSimpleQuery, Duration: time.Second, Accepted: false, TotalTokens: 999},
	}
	summary := summarizePerformanceSamples(samples)[TaskClassSimpleQuery]
	if summary.Samples != 3 || summary.Accepted != 2 || summary.P50Latency != 10*time.Millisecond || summary.P95Latency != 20*time.Millisecond || summary.P95TotalTokens != 20 {
		t.Fatalf("summary = %#v", summary)
	}
}

func BenchmarkEvaluatePerformanceGate(b *testing.B) {
	baseline := performanceSamples(100*time.Millisecond, 100, 2, 1, true)
	candidate := performanceSamples(90*time.Millisecond, 90, 2, 1, true)
	budget := DefaultPerformanceGateBudget()
	for b.Loop() {
		if _, err := EvaluatePerformanceGate(baseline, candidate, budget); err != nil {
			b.Fatal(err)
		}
	}
}

func performanceSamples(duration time.Duration, tokens, calls, retries int64, accepted bool) []PerformanceSample {
	samples := make([]PerformanceSample, 0, len(RequiredPerformanceTaskClasses)*5)
	for _, taskClass := range RequiredPerformanceTaskClasses {
		for range 5 {
			samples = append(samples, PerformanceSample{
				TaskClass: taskClass, Duration: duration, Accepted: accepted,
				TotalTokens: tokens, ProviderCalls: calls, Retries: retries,
			})
		}
	}
	return samples
}
