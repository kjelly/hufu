package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func newInspectEconomicsCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use: "economics <run-id>", Short: "Inspect observed generation cost and completion evidence",
		Args: cobra.ExactArgs(1), ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = args[0]
			envelope, inspectErr := inspectpkg.InspectEconomics(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func renderInspectEconomicsText(writer io.Writer, query inspectpkg.InspectQuery, data inspectpkg.EconomicsData) error {
	number := func(value *int64) string {
		if value == nil {
			return "unavailable"
		}
		return fmt.Sprint(*value)
	}
	_, err := fmt.Fprintf(writer,
		"Run: %s\nBranch: %s\nEvent head: %s\nComparison: %s\nOutcome: %s\nAcceptance: %s\nTasks: %d total, %d verified\nUsage cost subtotal (USD micros): %s\nCost per verified task (USD micros): %s\nUnavailable reasons: %s\nRecorded invocation coverage: %d/%d (does not prove historical coverage)\nInitial/retry/fallback (USD micros): %s / %s / %s\nBreakdown reason: %s\nOverhead usage subtotal (USD micros): %s\nUnassigned usage subtotal (USD micros): %s\nAttempt latency p50/p95 (ms): %s / %s; samples=%d excluded=%d\n",
		safeOverviewValue(query.RunID), safeOverviewValue(query.BranchID), safeOverviewValue(data.SourceHead.EventID),
		data.ComparisonScope, data.RunOutcome, data.Acceptance, data.TaskCount, data.VerifiedTaskCount,
		number(data.UsageCostMicros), number(data.CostPerVerifiedTaskMicros), strings.Join(data.CostPerVerifiedTaskReasons, ", "),
		data.UsageInvocations, data.RecordedInvocations, number(data.InitialCostMicros), number(data.RetryCostMicros),
		number(data.FallbackCostMicros), data.BreakdownReason, number(data.OverheadUsageCostMicros),
		number(data.UnassignedUsageCostMicros), number(data.Latency.P50Millis), number(data.Latency.P95Millis),
		data.Latency.SampleCount, data.Latency.ExcludedCount)
	if err != nil {
		return err
	}
	for _, group := range data.Routes {
		if _, err := fmt.Fprintf(writer, "Route: %s target=%s direct usage (USD micros)=%s reason=%s (excludes overhead)\n",
			safeOverviewValue(valueOrUnavailable(group.RouteName)), safeOverviewValue(valueOrUnavailable(group.ExecutionTarget)),
			number(group.UsageCostMicros), group.Reason); err != nil {
			return err
		}
	}
	return renderInspectCostText(writer, inspectpkg.CostData{View: data.Cost})
}
