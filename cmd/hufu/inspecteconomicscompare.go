package main

import (
	"fmt"
	"io"
	"strings"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/spf13/cobra"
)

func newInspectEconomicsCompareCommand(options *inspectCLIOptions) *cobra.Command {
	var manifest string
	command := &cobra.Command{Use: "economics-compare", Short: "Compare observed run costs in operator-declared groups", Args: cobra.NoArgs, ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			for _, flag := range []string{"workspace", "branch", "session"} {
				if flagChanged(command, flag) {
					return finishInspect(command, format, nil, fmt.Errorf("%w: economics-compare selects scope only through --manifest; --%s is not accepted", inspectpkg.ErrInvalidQuery, flag))
				}
			}
			if manifest == "" {
				return finishInspect(command, format, nil, fmt.Errorf("%w: --manifest is required", inspectpkg.ErrInvalidQuery))
			}
			result, inspectErr := inspectpkg.InspectEconomicsComparison(command.Context(), manifest)
			return finishInspect(command, format, result, inspectErr)
		}}
	command.Flags().StringVar(&manifest, "manifest", "", "Strict JSON comparison manifest (relative workspaces resolve from its directory)")
	return command
}

func renderInspectEconomicsComparisonText(writer io.Writer, data inspectpkg.EconomicsComparisonData) error {
	number := func(value *int64) string {
		if value == nil {
			return "unavailable"
		}
		return fmt.Sprint(*value)
	}
	ratio := func(value *float64) string {
		if value == nil {
			return "unavailable"
		}
		return fmt.Sprintf("%.6f", *value)
	}
	if _, err := fmt.Fprintf(writer, "Workload: %s (%s)\nComparison: %s\nObserved differences do not establish equivalent workloads or causal savings.\n", safeOverviewValue(data.WorkloadID), data.WorkloadSource, data.ComparisonScope); err != nil {
		return err
	}
	for _, observation := range data.SnapshotObservations {
		if _, err := fmt.Fprintf(writer, "Snapshot %s: observed=%s different=%t unavailable_runs=%d\n", observation.Field, safeOverviewValue(strings.Join(observation.ObservedValues, ", ")), observation.Different, observation.UnavailableRunCount); err != nil {
			return err
		}
	}
	for _, group := range data.Groups {
		if _, err := fmt.Fprintf(writer, "\nGroup: %s\nRuns: %d total, %d terminal, %d nonterminal, %d accepted; accepted rate=%s\nOutcomes: %v\nAcceptance states: %v\nTasks: %d total, %d verified; verified rate=%s\nKnown subtotal (USD micros): %s\nTotal cost (USD micros): %s\nCost per accepted run (USD micros): %s\nIncomplete cost runs: %d; reasons=%v\nCost per accepted run reasons: %s\n", safeOverviewValue(group.Group), group.RunCount, group.TerminalRunCount, group.NonterminalRunCount, group.AcceptedRunCount, ratio(group.AcceptedRate), group.OutcomeCounts, group.AcceptanceCounts, group.TaskCount, group.VerifiedTaskCount, ratio(group.VerifiedTaskRate), number(group.KnownSubtotalMicros), number(group.TotalCostMicros), number(group.CostPerAcceptedRunMicros), group.IncompleteCostRunCount, group.CostUnavailableReasons, strings.Join(group.CostPerAcceptedRunReasons, ", ")); err != nil {
			return err
		}
	}
	for _, run := range data.Runs {
		if _, err := fmt.Fprintf(writer, "\nGroup: %s\nWorkspace: %s\nSession: %s\nTerminal: %t\nSource hash: %s\nPolicy: %s\nInput snapshot: %s hash=%s schema=%s\nSnapshot unavailable: %s\nCost completeness reasons: %s\n", safeOverviewValue(run.Selection.Group), safeOverviewValue(run.Selection.Workspace), safeOverviewValue(run.Scope.SessionID), run.Terminal, safeOverviewValue(run.Economics.SourceHead.EventHash), safeOverviewValue(valueOrUnavailable(run.Snapshots.PolicyHash)), safeOverviewValue(valueOrUnavailable(run.Snapshots.InputSnapshotID)), safeOverviewValue(valueOrUnavailable(run.Snapshots.InputSnapshotHash)), safeOverviewValue(valueOrUnavailable(run.Snapshots.InputSchemaHash)), strings.Join(run.Snapshots.Unavailable, ", "), strings.Join(run.CostReasons, ", ")); err != nil {
			return err
		}
		if err := renderInspectEconomicsText(writer, inspectpkg.InspectQuery{RunID: run.Scope.RunID, BranchID: run.Scope.BranchID}, run.Economics); err != nil {
			return err
		}
	}
	return nil
}
