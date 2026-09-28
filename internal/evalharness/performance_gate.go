package evalharness

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"time"
)

// PerformanceTaskClass is a stable workload category used by the end-to-end
// performance gate. Every gate comparison must cover all five classes so a
// speedup in trivial work cannot hide a regression in verification or recovery.
type PerformanceTaskClass string

const (
	TaskClassSimpleQuery       PerformanceTaskClass = "simple-query"
	TaskClassLocalizedChange   PerformanceTaskClass = "localized-change"
	TaskClassVerification      PerformanceTaskClass = "verification"
	TaskClassMultiRoleWorkflow PerformanceTaskClass = "multi-role-workflow"
	TaskClassFailureRecovery   PerformanceTaskClass = "failure-recovery"
)

// RequiredPerformanceTaskClasses is the minimum representative workload set.
var RequiredPerformanceTaskClasses = []PerformanceTaskClass{
	TaskClassSimpleQuery,
	TaskClassLocalizedChange,
	TaskClassVerification,
	TaskClassMultiRoleWorkflow,
	TaskClassFailureRecovery,
}

// PerformanceSample is one end-to-end invocation observation. Baseline samples
// are expected to come from the single-agent path; candidate samples come from
// the Hufu route under evaluation. Duration and resource metrics are considered
// only for accepted samples, while AcceptanceRate includes every sample.
type PerformanceSample struct {
	TaskClass     PerformanceTaskClass `json:"task_class"`
	Duration      time.Duration        `json:"duration_ns"`
	Accepted      bool                 `json:"accepted"`
	TotalTokens   int64                `json:"total_tokens"`
	ProviderCalls int64                `json:"provider_calls"`
	Retries       int64                `json:"retries"`
}

// PerformanceGateBudget bounds candidate regressions relative to the
// single-agent baseline. Ratios must be at least 1. A zero-value budget uses
// DefaultPerformanceGateBudget.
type PerformanceGateBudget struct {
	MaxP95LatencyRatio       float64
	MaxP95TokensRatio        float64
	MaxP95ProviderCallsRatio float64
	MaxP95RetriesRatio       float64
	MaxAcceptanceRateDrop    float64
	MinSamplesPerClass       int
}

// DefaultPerformanceGateBudget permits modest host-level latency variance but
// no token, call, retry, or correctness regression.
func DefaultPerformanceGateBudget() PerformanceGateBudget {
	return PerformanceGateBudget{
		MaxP95LatencyRatio:       1.25,
		MaxP95TokensRatio:        1,
		MaxP95ProviderCallsRatio: 1,
		MaxP95RetriesRatio:       1,
		MaxAcceptanceRateDrop:    0,
		MinSamplesPerClass:       5,
	}
}

// PerformanceClassSummary is the accepted-task latency/resource distribution
// plus whole-class correctness rate.
type PerformanceClassSummary struct {
	Samples          int           `json:"samples"`
	Accepted         int           `json:"accepted"`
	AcceptanceRate   float64       `json:"acceptance_rate"`
	P50Latency       time.Duration `json:"p50_latency_ns"`
	P95Latency       time.Duration `json:"p95_latency_ns"`
	P95TotalTokens   int64         `json:"p95_total_tokens"`
	P95ProviderCalls int64         `json:"p95_provider_calls"`
	P95Retries       int64         `json:"p95_retries"`
}

// PerformanceGateFinding identifies one missing workload or regression.
type PerformanceGateFinding struct {
	TaskClass PerformanceTaskClass `json:"task_class"`
	Metric    string               `json:"metric"`
	Baseline  string               `json:"baseline,omitempty"`
	Candidate string               `json:"candidate,omitempty"`
	Limit     string               `json:"limit"`
}

// PerformanceGateReport contains comparable summaries and every blocking
// finding. Passed is true only when correctness is non-regressing and all
// configured performance budgets hold for every required task class.
type PerformanceGateReport struct {
	Passed    bool                                             `json:"passed"`
	Baseline  map[PerformanceTaskClass]PerformanceClassSummary `json:"baseline"`
	Candidate map[PerformanceTaskClass]PerformanceClassSummary `json:"candidate"`
	Findings  []PerformanceGateFinding                         `json:"findings"`
}

// LoadPerformanceSamples strict-decodes a benchmark artifact. Duration is
// represented as nanoseconds so artifacts retain exact measurements.
func LoadPerformanceSamples(path string) ([]PerformanceSample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open performance samples: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var samples []PerformanceSample
	if err := decoder.Decode(&samples); err != nil {
		return nil, fmt.Errorf("decode performance samples: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("performance samples contain a trailing JSON value")
		}
		return nil, fmt.Errorf("decode trailing performance sample data: %w", err)
	}
	if err := validatePerformanceSamples("loaded", samples); err != nil {
		return nil, err
	}
	return samples, nil
}

// WritePerformanceGateReport writes the machine-readable blocking-gate result.
func WritePerformanceGateReport(writer io.Writer, report PerformanceGateReport) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode performance gate report: %w", err)
	}
	return nil
}

// EvaluatePerformanceGate compares Hufu end-to-end samples against a
// single-agent baseline without allowing performance to trade away acceptance.
func EvaluatePerformanceGate(baseline, candidate []PerformanceSample, budget PerformanceGateBudget) (PerformanceGateReport, error) {
	if budget == (PerformanceGateBudget{}) {
		budget = DefaultPerformanceGateBudget()
	}
	if err := validatePerformanceGateBudget(budget); err != nil {
		return PerformanceGateReport{}, err
	}
	if err := validatePerformanceSamples("baseline", baseline); err != nil {
		return PerformanceGateReport{}, err
	}
	if err := validatePerformanceSamples("candidate", candidate); err != nil {
		return PerformanceGateReport{}, err
	}

	report := PerformanceGateReport{
		Baseline:  summarizePerformanceSamples(baseline),
		Candidate: summarizePerformanceSamples(candidate),
	}
	for _, taskClass := range RequiredPerformanceTaskClasses {
		base, baseOK := report.Baseline[taskClass]
		cand, candOK := report.Candidate[taskClass]
		if !baseOK || base.Samples < budget.MinSamplesPerClass {
			report.Findings = append(report.Findings, missingPerformanceClassFinding(taskClass, "baseline", base.Samples, budget.MinSamplesPerClass))
			continue
		}
		if !candOK || cand.Samples < budget.MinSamplesPerClass {
			report.Findings = append(report.Findings, missingPerformanceClassFinding(taskClass, "candidate", cand.Samples, budget.MinSamplesPerClass))
			continue
		}
		if cand.AcceptanceRate+budget.MaxAcceptanceRateDrop < base.AcceptanceRate {
			report.Findings = append(report.Findings, PerformanceGateFinding{
				TaskClass: taskClass,
				Metric:    "acceptance-rate",
				Baseline:  fmt.Sprintf("%.4f", base.AcceptanceRate),
				Candidate: fmt.Sprintf("%.4f", cand.AcceptanceRate),
				Limit:     fmt.Sprintf("drop <= %.4f", budget.MaxAcceptanceRateDrop),
			})
		}
		if base.Accepted == 0 || cand.Accepted == 0 {
			report.Findings = append(report.Findings, PerformanceGateFinding{
				TaskClass: taskClass,
				Metric:    "accepted-samples",
				Baseline:  fmt.Sprint(base.Accepted),
				Candidate: fmt.Sprint(cand.Accepted),
				Limit:     "both must be greater than zero",
			})
			continue
		}
		report.Findings = appendRatioRegression(report.Findings, taskClass, "p95-latency", int64(base.P95Latency), int64(cand.P95Latency), budget.MaxP95LatencyRatio, base.P95Latency.String, cand.P95Latency.String)
		report.Findings = appendRatioRegression(report.Findings, taskClass, "p95-total-tokens", base.P95TotalTokens, cand.P95TotalTokens, budget.MaxP95TokensRatio, func() string { return fmt.Sprint(base.P95TotalTokens) }, func() string { return fmt.Sprint(cand.P95TotalTokens) })
		report.Findings = appendRatioRegression(report.Findings, taskClass, "p95-provider-calls", base.P95ProviderCalls, cand.P95ProviderCalls, budget.MaxP95ProviderCallsRatio, func() string { return fmt.Sprint(base.P95ProviderCalls) }, func() string { return fmt.Sprint(cand.P95ProviderCalls) })
		report.Findings = appendRatioRegression(report.Findings, taskClass, "p95-retries", base.P95Retries, cand.P95Retries, budget.MaxP95RetriesRatio, func() string { return fmt.Sprint(base.P95Retries) }, func() string { return fmt.Sprint(cand.P95Retries) })
	}
	report.Passed = len(report.Findings) == 0
	return report, nil
}

func validatePerformanceSamples(label string, samples []PerformanceSample) error {
	for index, sample := range samples {
		if !slices.Contains(RequiredPerformanceTaskClasses, sample.TaskClass) {
			return fmt.Errorf("%s performance sample %d has unknown task class %q", label, index, sample.TaskClass)
		}
		if sample.Duration < 0 || sample.TotalTokens < 0 || sample.ProviderCalls < 0 || sample.Retries < 0 {
			return fmt.Errorf("%s performance sample %d contains a negative metric", label, index)
		}
	}
	return nil
}

func validatePerformanceGateBudget(budget PerformanceGateBudget) error {
	for name, ratio := range map[string]float64{
		"latency":        budget.MaxP95LatencyRatio,
		"tokens":         budget.MaxP95TokensRatio,
		"provider calls": budget.MaxP95ProviderCallsRatio,
		"retries":        budget.MaxP95RetriesRatio,
	} {
		if ratio < 1 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			return fmt.Errorf("performance gate %s ratio must be at least 1", name)
		}
	}
	if budget.MaxAcceptanceRateDrop < 0 || budget.MaxAcceptanceRateDrop > 1 ||
		math.IsNaN(budget.MaxAcceptanceRateDrop) || math.IsInf(budget.MaxAcceptanceRateDrop, 0) {
		return fmt.Errorf("performance gate acceptance-rate drop must be between 0 and 1")
	}
	if budget.MinSamplesPerClass < 1 {
		return fmt.Errorf("performance gate minimum samples per class must be positive")
	}
	return nil
}

func summarizePerformanceSamples(samples []PerformanceSample) map[PerformanceTaskClass]PerformanceClassSummary {
	grouped := make(map[PerformanceTaskClass][]PerformanceSample)
	for _, sample := range samples {
		grouped[sample.TaskClass] = append(grouped[sample.TaskClass], sample)
	}
	summaries := make(map[PerformanceTaskClass]PerformanceClassSummary, len(grouped))
	for taskClass, classSamples := range grouped {
		var durations []time.Duration
		var tokens, calls, retries []int64
		for _, sample := range classSamples {
			if !sample.Accepted {
				continue
			}
			durations = append(durations, sample.Duration)
			tokens = append(tokens, sample.TotalTokens)
			calls = append(calls, sample.ProviderCalls)
			retries = append(retries, sample.Retries)
		}
		summaries[taskClass] = PerformanceClassSummary{
			Samples:          len(classSamples),
			Accepted:         len(durations),
			AcceptanceRate:   float64(len(durations)) / float64(len(classSamples)),
			P50Latency:       percentileDuration(durations, 0.50),
			P95Latency:       percentileDuration(durations, 0.95),
			P95TotalTokens:   percentileInt64(tokens, 0.95),
			P95ProviderCalls: percentileInt64(calls, 0.95),
			P95Retries:       percentileInt64(retries, 0.95),
		}
	}
	return summaries
}

func percentileDuration(values []time.Duration, percentile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return ordered[percentileIndex(len(ordered), percentile)]
}

func percentileInt64(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return ordered[percentileIndex(len(ordered), percentile)]
}

func percentileIndex(length int, percentile float64) int {
	index := int(math.Ceil(float64(length)*percentile)) - 1
	return min(max(index, 0), length-1)
}

func missingPerformanceClassFinding(taskClass PerformanceTaskClass, side string, samples, minimum int) PerformanceGateFinding {
	finding := PerformanceGateFinding{
		TaskClass: taskClass,
		Metric:    side + "-samples",
		Limit:     fmt.Sprintf(">= %d", minimum),
	}
	if side == "baseline" {
		finding.Baseline = fmt.Sprint(samples)
	} else {
		finding.Candidate = fmt.Sprint(samples)
	}
	return finding
}

func appendRatioRegression(findings []PerformanceGateFinding, taskClass PerformanceTaskClass, metric string, baseline, candidate int64, ratio float64, baselineText, candidateText func() string) []PerformanceGateFinding {
	limit := float64(baseline) * ratio
	if float64(candidate) <= limit {
		return findings
	}
	return append(findings, PerformanceGateFinding{
		TaskClass: taskClass,
		Metric:    metric,
		Baseline:  baselineText(),
		Candidate: candidateText(),
		Limit:     fmt.Sprintf("<= %.2fx baseline", ratio),
	})
}
