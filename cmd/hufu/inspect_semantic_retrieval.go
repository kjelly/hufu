package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

// inspectSemanticTaskRows bounds the per-task table in text output; JSON
// always has every row.
const inspectSemanticTaskRows = 40

func newInspectSemanticRetrievalCommand(options *inspectCLIOptions) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use:   "semantic-retrieval [run-id]",
		Short: "Summarize semantic memory retrieval: routes, fallbacks, latency, and the runtime state",
		Long: `Summarize the workspace's content-free semantic retrieval traces
(semantic-retrieval-traces.jsonl) for every run, one run, or one task: how
many retrievals each memory ranking (shared_persistent, worker_private) sent
to the semantic lane or kept lexical in auto mode, how many the lane served
without a fallback, the fallback reasons, truncated queries, and latency,
queue-wait, and embedding percentiles. It also shows the runtime status
recorded when the run finished: warm state, model, generation, and that
run's query, route, and refresh counts.
Traces hold only HMAC hashes, counts, and identities; never query or memory text.`,
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
			query.TaskID = taskID
			envelope, inspectErr := inspectpkg.InspectSemanticRetrieval(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Optional exact task ID filter")
	return command
}

func renderInspectSemanticRetrievalText(writer io.Writer, query inspectpkg.InspectQuery, data inspectpkg.SemanticRetrievalData) error {
	scope := "all runs"
	if query.RunID != "" {
		scope = "run " + safeOverviewValue(query.RunID)
	}
	if query.TaskID != "" {
		scope += ", task " + safeOverviewValue(query.TaskID)
	}
	if _, err := fmt.Fprintf(writer, "Semantic retrieval (%s)\n", scope); err != nil {
		return err
	}
	if data.Runtime != nil {
		if err := renderSemanticRuntimeText(writer, *data.Runtime); err != nil {
			return err
		}
	}
	summary := data.Summary
	if summary.Traces == 0 {
		_, err := fmt.Fprintln(writer, "No semantic retrieval traces.")
		return err
	}
	if _, err := fmt.Fprintf(writer, "Traces: %d across %d run(s)\n", summary.Traces, summary.Runs); err != nil {
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "SOURCE\tROUTE\tMODE\tRETRIEVALS\tSERVED\tFALLBACKS\tTRUNCATED\tP50/P95 MS\tP95 QUEUE MS\tP50/P95 EMBED MS"); err != nil {
		return err
	}
	for _, route := range summary.Routes {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d/%d\t%d\t%d/%d\n",
			safeOverviewValue(valueOrUnavailable(route.Source)), safeOverviewValue(route.Route), safeOverviewValue(route.Mode),
			route.Retrievals, route.Served, route.Fallbacks, route.Truncated, route.P50LatencyMS, route.P95LatencyMS,
			route.P95QueueWaitMS, route.P50EmbedMS, route.P95EmbedMS); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if len(summary.Fallbacks) > 0 {
		if _, err := fmt.Fprintf(writer, "Fallback reasons: %s\n", formatSemanticCounts(summary.Fallbacks)); err != nil {
			return err
		}
	}
	if err := renderSemanticTasksText(writer, summary.Tasks); err != nil {
		return err
	}
	if data.SkippedLines > 0 {
		_, err := fmt.Fprintf(writer, "Skipped %d unreadable trace or status line(s).\n", data.SkippedLines)
		return err
	}
	return nil
}

func renderSemanticRuntimeText(writer io.Writer, record team.SemanticRuntimeRecord) error {
	status := record.Status
	if _, err := fmt.Fprintf(writer, "Runtime when run %s finished (%s): mode %s, state %s, model %s, generation %s\n",
		safeOverviewValue(valueOrUnavailable(record.RunID)), record.RecordedAt.Format("2006-01-02 15:04:05Z"),
		safeOverviewValue(status.Mode), safeOverviewValue(status.State), safeOverviewValue(status.Model),
		safeOverviewValue(valueOrUnavailable(status.GenerationID))); err != nil {
		return err
	}
	line := fmt.Sprintf("  queries %d, served %d, truncated %d, warmups %d, refreshes %d (%d failed)",
		status.Queries, status.SemanticResults, status.TruncatedQueries, status.Warmups, status.Refreshes, status.RefreshFailures)
	if status.RoutedSemantic+status.RoutedLexical > 0 {
		line += fmt.Sprintf(", routed semantic %d / lexical %d", status.RoutedSemantic, status.RoutedLexical)
	}
	if status.Failure != "" {
		line += ", failure " + safeOverviewValue(status.Failure)
	}
	_, err := fmt.Fprintln(writer, line)
	return err
}

func renderSemanticTasksText(writer io.Writer, tasks []team.SemanticTaskSummary) error {
	if len(tasks) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(writer, "Tasks:"); err != nil {
		return err
	}
	shown := tasks
	if len(shown) > inspectSemanticTaskRows {
		shown = shown[len(shown)-inspectSemanticTaskRows:]
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "RUN\tTASK\tATTEMPT\tPHASE\tAGENT\tSOURCE\tROUTE\tSHARE\tRETRIEVALS\tSERVED\tFALLBACKS"); err != nil {
		return err
	}
	for _, task := range shown {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%.2f\t%d\t%d\t%s\n",
			safeOverviewValue(valueOrUnavailable(task.RunID)), safeOverviewValue(valueOrUnavailable(task.TaskID)), task.Attempt,
			safeOverviewValue(valueOrUnavailable(task.Phase)), safeOverviewValue(valueOrUnavailable(task.Agent)),
			safeOverviewValue(valueOrUnavailable(task.Source)), safeOverviewValue(task.Route), task.Share,
			task.Retrievals, task.Served, formatSemanticCounts(task.Fallbacks)); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if hidden := len(tasks) - len(shown); hidden > 0 {
		_, err := fmt.Fprintf(writer, "%d earlier task row(s) not shown; use --format json for all.\n", hidden)
		return err
	}
	return nil
}

// formatSemanticCounts renders reason=count pairs in reason order, or "-".
func formatSemanticCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(counts))
	for _, reason := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", safeOverviewValue(reason), counts[reason]))
	}
	return strings.Join(parts, ", ")
}
