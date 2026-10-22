package inspect

import (
	"context"
	"slices"

	"github.com/kjelly/hufu/internal/cost"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

func overviewObservations(ctx context.Context, query InspectQuery, lineage Lineage, head cost.Freshness) *operatorpkg.ObservationSummary {
	data, err := projectEconomics(ctx, query, lineage)
	if err != nil {
		return &operatorpkg.ObservationSummary{
			Status: "unavailable", SourceHead: head, ComparisonScope: "single_run_observed",
			ReasonCodes: []string{"evidence_unavailable"}, Routes: []operatorpkg.ObservedRoute{},
		}
	}
	summary := &operatorpkg.ObservationSummary{
		Status: "available", SourceHead: data.SourceHead, ComparisonScope: data.ComparisonScope,
		TaskCount: new(data.TaskCount), VerifiedTasks: new(data.VerifiedTaskCount),
		UnavailableTasks: new(data.EvidenceCoverage.Unavailable),
		UsageCostMicros:  data.UsageCostMicros, KnownCostCoverage: data.KnownCostCoverage,
		ReasonCodes: slices.Clone(data.CostPerVerifiedTaskReasons), Routes: []operatorpkg.ObservedRoute{},
	}
	if data.BreakdownReason != "" {
		summary.ReasonCodes = append(summary.ReasonCodes, data.BreakdownReason)
	}
	for _, diagnostic := range data.Diagnostics {
		summary.ReasonCodes = append(summary.ReasonCodes, diagnostic.Code)
	}
	slices.Sort(summary.ReasonCodes)
	summary.ReasonCodes = slices.Compact(summary.ReasonCodes)
	for _, route := range data.Routes {
		summary.Routes = append(summary.Routes, operatorpkg.ObservedRoute{
			Kind: route.RouteKind, Name: route.RouteName, Digest: route.RouteDigest,
			Target: route.ExecutionTarget, Attempts: route.Attempts, VerifiedTasks: route.VerifiedTasks,
		})
	}
	return summary
}
