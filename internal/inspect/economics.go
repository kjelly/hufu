package inspect

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/team"
)

// EconomicsData describes one observed run, not a comparison experiment.
// Null amounts are intentional; UsageCostMicros is only a known subtotal.
type EconomicsData struct {
	SchemaVersion              int              `json:"schema_version"`
	ComparisonScope            string           `json:"comparison_scope"`
	SourceHead                 cost.Freshness   `json:"source_head"`
	RunOutcome                 string           `json:"run_outcome"`
	Acceptance                 string           `json:"acceptance_state"`
	TaskCount                  int              `json:"task_count"`
	TaskStates                 map[string]int   `json:"task_states"`
	VerifiedTaskCount          int              `json:"verified_task_count"`
	VerifiedTaskRate           *float64         `json:"verified_task_rate"`
	EvidenceCoverage           RouteCoverage    `json:"evidence_coverage"`
	UsageCostMicros            *int64           `json:"usage_cost_micros"`
	CostPerVerifiedTaskMicros  *int64           `json:"cost_per_verified_task_micros"`
	CostPerVerifiedTaskReasons []string         `json:"cost_per_verified_task_reasons"`
	KnownCostCoverage          *float64         `json:"known_cost_coverage"`
	KnownCostCoverageScope     string           `json:"known_cost_coverage_scope"`
	RecordedInvocations        int              `json:"recorded_invocations"`
	UsageInvocations           int              `json:"usage_invocations"`
	Cost                       cost.View        `json:"cost"`
	InitialCostMicros          *int64           `json:"initial_cost_micros"`
	RetryCostMicros            *int64           `json:"retry_cost_micros"`
	FallbackCostMicros         *int64           `json:"fallback_cost_micros"`
	BreakdownReason            string           `json:"breakdown_reason,omitempty"`
	OverheadUsageCostMicros    *int64           `json:"overhead_usage_cost_micros"`
	UnassignedUsageCostMicros  *int64           `json:"unassigned_usage_cost_micros"`
	UnassignedInvocations      int              `json:"unassigned_invocations"`
	Occurrences                []OccurrenceCost `json:"occurrences"`
	Routes                     []RouteEconomics `json:"routes"`
	Latency                    AttemptLatency   `json:"attempt_latency"`
	Diagnostics                []Diagnostic     `json:"diagnostics"`
}

type OccurrenceCost struct {
	TaskID            string `json:"task_id"`
	OccurrenceAttempt int    `json:"occurrence_attempt"`
	UsageCostMicros   *int64 `json:"usage_cost_micros"`
}

type RouteEconomics struct {
	RouteGroup
	UsageCostMicros  *int64 `json:"usage_cost_micros"`
	Reason           string `json:"reason,omitempty"`
	ExcludesOverhead bool   `json:"excludes_overhead"`
}

type AttemptLatency struct {
	P50Millis     *int64 `json:"p50_attempt_latency_ms"`
	P95Millis     *int64 `json:"p95_attempt_latency_ms"`
	SampleCount   int    `json:"sample_count"`
	ExcludedCount int    `json:"excluded_count"`
	Reason        string `json:"reason,omitempty"`
}

func InspectEconomics(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindEconomics); err != nil {
		return nil, err
	}
	resolved, bound, err := bindObservationTarget(ctx, query)
	if err != nil {
		return nil, err
	}
	data, err := projectEconomics(ctx, resolved, bound.Lineage)
	if err != nil {
		return nil, err
	}
	return envelope(KindEconomics, resolved, bound.Scope.BranchID, data), nil
}

func projectEconomics(ctx context.Context, query InspectQuery, lineage Lineage) (EconomicsData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	routes, err := projectRoutes(ctx, query, lineage)
	if err != nil {
		return EconomicsData{}, err
	}
	events, err := observationEvents(lineage, query)
	if err != nil {
		return EconomicsData{}, err
	}
	raw := make([]team.RunEvent, len(events))
	for i, event := range events {
		raw[i] = event.Event
	}
	ledger, policy, err := team.ProjectCostLedger(raw)
	if err != nil {
		return EconomicsData{}, fmt.Errorf("%w: economics ledger: %v", ErrIntegrity, err)
	}
	// Ancestor events establish policy and identity, never this run's cost.
	scoped := cost.NewProjection()
	scoped.Prices = ledger.Prices
	scoped.Denials = ledger.Denials
	for key, invocation := range ledger.Invocations {
		if invocation.Reservation.RunID == query.RunID {
			scoped.Invocations[key] = invocation
		}
	}
	for _, event := range raw {
		if event.RunID != query.RunID {
			continue
		}
		switch team.EventType(event.Type) {
		case team.EventCostPriceSnapshotResolved, team.EventCostReservationCommitted, team.EventCostSettled,
			team.EventCostBudgetWarning, team.EventCostBudgetDenied:
			scoped.Available = true
		}
	}
	var costPolicy *cost.PolicySnapshot
	if policy != nil {
		costPolicy = policy.Cost
	}
	view, err := cost.BuildView(scoped, costPolicy, query.RunID, "")
	if err != nil {
		return EconomicsData{}, fmt.Errorf("%w: economics amounts: %v", ErrIntegrity, err)
	}
	view.Freshness = routes.SourceHead
	data := EconomicsData{
		SchemaVersion: 1, ComparisonScope: "single_run_observed", SourceHead: routes.SourceHead,
		RunOutcome: routes.RunOutcome, Acceptance: routes.Acceptance, Cost: view,
		TaskCount: routes.Coverage.Tasks, VerifiedTaskCount: routes.Coverage.Verified,
		EvidenceCoverage: routes.Coverage, TaskStates: map[string]int{},
		UsageCostMicros: view.UsageDerivedMicros, KnownCostCoverageScope: "recorded_invocations",
		RecordedInvocations: len(scoped.Invocations), CostPerVerifiedTaskReasons: []string{},
		Occurrences: []OccurrenceCost{}, Routes: []RouteEconomics{}, Latency: economicsLatency(routes.Attempts),
		Diagnostics: slices.Clone(routes.Diagnostics),
	}
	for _, task := range routes.Tasks {
		if task.ChangedInRun {
			data.TaskStates[task.TaskStatus]++
		}
	}
	if data.TaskCount > 0 {
		data.VerifiedTaskRate = new(float64(data.VerifiedTaskCount) / float64(data.TaskCount))
	}
	for _, invocation := range scoped.Invocations {
		if usageAmount(invocation) != nil {
			data.UsageInvocations++
		}
	}
	if data.RecordedInvocations > 0 {
		data.KnownCostCoverage = new(float64(data.UsageInvocations) / float64(data.RecordedInvocations))
	}
	if err := economicsAttributionContext(ctx, &data, routes, scoped); err != nil {
		return EconomicsData{}, err
	}
	data.CostPerVerifiedTaskReasons = economicsCompleteness(query.RunID, raw, policy, scoped, routes, data)
	if len(data.CostPerVerifiedTaskReasons) == 0 {
		total, denominator := *data.UsageCostMicros, int64(data.VerifiedTaskCount)
		rounded := total / denominator
		if total%denominator != 0 {
			rounded++
		}
		data.CostPerVerifiedTaskMicros = new(rounded)
	}
	return data, nil
}

func usageAmount(invocation cost.InvocationProjection) *int64 {
	if invocation.Settlement != nil && invocation.Settlement.EstimateSource == cost.EstimateUsage {
		return invocation.Settlement.FinalMicros
	}
	return nil
}

func economicsLatency(attempts []RouteAttempt) AttemptLatency {
	result := AttemptLatency{}
	var samples []int64
	for _, attempt := range attempts {
		if !attempt.Counted {
			continue
		}
		if attempt.StartedAt.IsZero() || attempt.FinishedAt.IsZero() ||
			attempt.FinishedAt.Before(attempt.StartedAt) {
			result.ExcludedCount++
			continue
		}
		samples = append(samples, attempt.FinishedAt.Sub(attempt.StartedAt).Milliseconds())
	}
	result.SampleCount = len(samples)
	if len(samples) == 0 {
		result.Reason = "no_samples"
		return result
	}
	slices.Sort(samples)
	// ceil(n*p/100), avoiding multiplication overflow for large n.
	rank := func(percent int) int {
		return len(samples)/100*percent + (len(samples)%100*percent+99)/100 - 1
	}
	result.P50Millis, result.P95Millis = new(samples[rank(50)]), new(samples[rank(95)])
	return result
}

func sortedInvocations(ledger cost.Projection) []cost.InvocationProjection {
	result := make([]cost.InvocationProjection, 0, len(ledger.Invocations))
	for _, key := range slices.Sorted(maps.Keys(ledger.Invocations)) {
		result = append(result, ledger.Invocations[key])
	}
	return result
}
