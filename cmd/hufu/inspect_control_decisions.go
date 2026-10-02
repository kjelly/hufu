package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func newInspectControlDecisionsCommand(options *inspectCLIOptions) *cobra.Command {
	var allBranches bool
	command := &cobra.Command{
		Use:   "control-decisions [run-id]",
		Short: "Summarize runtime control decision observations (shadow agreement, abstentions, latency)",
		Long: `Summarize control_decision_observed events per point and mode across every
run in the branch lineage, every branch (--all-branches; each --new run starts
a new branch), or one run. Use it to decide whether a point can
move from shadow to active: agreement with the existing path, how often the
decision model would abstain under the point's threshold, errors, and latency.
Confidence is the decision model's raw probability, not a calibrated accuracy;
backend sidecar reports none.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				query.RunID = args[0]
			}
			query.AllBranches = allBranches
			envelope, inspectErr := inspectpkg.InspectControlDecisions(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
	command.Flags().BoolVar(&allBranches, "all-branches", false, "Aggregate every branch in the workspace, including earlier --new sessions")
	return command
}

func renderInspectControlDecisionsText(writer io.Writer, query inspectpkg.InspectQuery, data inspectpkg.ControlDecisionsData) error {
	scope := "all runs"
	if query.RunID != "" {
		scope = "run " + safeOverviewValue(query.RunID)
	}
	branch := "branch " + safeOverviewValue(valueOrUnavailable(query.BranchID))
	if data.Scope == "all_branches" {
		branch = "all branches"
	}
	if _, err := fmt.Fprintf(writer, "Control decisions (%s, %s)\n", scope, branch); err != nil {
		return err
	}
	if len(data.Summaries) == 0 {
		_, err := fmt.Fprintln(writer, "No control decision observations.")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "POINT\tMODE\tBACKEND\tCALLS\tAGREED\tBELOW-THRESHOLD\tERRORS\tMEAN-CONF\tP50/P95 MS\tAPPLIED decision/safe/existing"); err != nil {
		return err
	}
	for _, summary := range data.Summaries {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d/%d\t%d\t%d\t%.3f\t%d/%d\t%d/%d/%d\n",
			safeOverviewValue(summary.Point), safeOverviewValue(summary.Mode), safeOverviewValue(summary.Backend), summary.Calls, summary.Agreed, summary.Compared,
			summary.BelowThreshold, summary.Errors, summary.MeanConfidence, summary.P50MS, summary.P95MS,
			summary.AppliedPrimitive, summary.AppliedSafeDefault, summary.AppliedLegacy); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(writer, "Confidence is the decision model's raw probability, not a calibrated accuracy; backend sidecar reports none.")
	return err
}
