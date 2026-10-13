package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// writeSemanticRetrievalRuntimeReport renders the semantic retrieval runtime
// state and this run's routes (docs/reference/semantic-retrieval.md). It
// writes nothing when semantic retrieval is off.
func writeSemanticRetrievalRuntimeReport(b *strings.Builder, data *team.SemanticRetrievalReportData) {
	if data == nil {
		return
	}
	status := data.Status
	b.WriteString("## Semantic Retrieval Runtime\n\n")
	fmt.Fprintf(b, "Run `%s`: mode `%s`, state `%s`, model `%s`, generation `%s`. Queries %d, served %d, truncated %d; warmups %d, refreshes %d (%d failed).",
		reportSafeMetadata(valueOrUnavailable(data.RunID), 120), reportSafeMetadata(status.Mode, 40), reportSafeMetadata(status.State, 40), reportSafeMetadata(status.Model, 120),
		reportSafeMetadata(valueOrUnavailable(status.GenerationID), 120), status.Queries, status.SemanticResults, status.TruncatedQueries,
		status.Warmups, status.Refreshes, status.RefreshFailures)
	if status.RoutedSemantic+status.RoutedLexical > 0 {
		fmt.Fprintf(b, " Auto routed %d request(s) semantic and %d lexical.", status.RoutedSemantic, status.RoutedLexical)
	}
	if status.Failure != "" {
		fmt.Fprintf(b, " Failure: `%s`.", reportSafeMetadata(status.Failure, 80))
	}
	b.WriteString("\n\n")
	summary := data.Summary
	if len(summary.Routes) > 0 {
		b.WriteString("| Source | Route | Mode | Requests | Served | Fallbacks | Truncated | p50 / p95 (ms) | p95 queue (ms) | p50 / p95 embed (ms) |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, route := range summary.Routes {
			fmt.Fprintf(b, "| %s | %s | %s | %d | %d | %d | %d | %d / %d | %d | %d / %d |\n",
				reportSafeMetadata(valueOrUnavailable(route.Source), 40), reportSafeMetadata(route.Route, 20), reportSafeMetadata(route.Mode, 20),
				route.Requests, route.Served, route.Fallbacks, route.Truncated, route.P50LatencyMS, route.P95LatencyMS,
				route.P95QueueWaitMS, route.P50EmbedMS, route.P95EmbedMS)
		}
		b.WriteString("\n")
	}
	if len(summary.Fallbacks) > 0 {
		fmt.Fprintf(b, "Fallback reasons: %s.\n\n", formatSemanticCounts(summary.Fallbacks))
	}
	b.WriteString("Per-task routes: `hufu inspect semantic-retrieval <run-id>`.\n\n")
}
