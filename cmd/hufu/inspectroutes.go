package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func newInspectRoutesCommand(options *inspectCLIOptions) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use: "routes <run-id>", Short: "Inspect recorded worker routes and completion evidence",
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
			query.RunID, query.TaskID = args[0], taskID
			envelope, inspectErr := inspectpkg.InspectRoutes(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Optional exact task ID filter")
	return command
}

func renderInspectRoutesText(writer io.Writer, query inspectpkg.InspectQuery, data inspectpkg.RoutesData) error {
	if _, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\nEvent head: %s\nOutcome: %s\nAcceptance: %s\nTasks: %d total, %d verified, %d not verified, %d unavailable, %d reused\nAttempts: %d\n",
		safeOverviewValue(query.RunID), safeOverviewValue(query.BranchID), safeOverviewValue(data.SourceHead.EventID),
		data.RunOutcome, data.Acceptance, data.Coverage.Tasks, data.Coverage.Verified,
		data.Coverage.NotVerified, data.Coverage.Unavailable, data.Coverage.Reused, data.AttemptCount); err != nil {
		return err
	}
	for _, group := range data.Groups {
		if _, err := fmt.Fprintf(writer, "Route: %s (%s) target=%s attempts=%d verified=%d digest=%s\n",
			safeOverviewValue(valueOrUnavailable(group.RouteName)), group.RouteKind,
			safeOverviewValue(valueOrUnavailable(group.ExecutionTarget)), group.Attempts, group.VerifiedTasks,
			safeOverviewValue(valueOrUnavailable(group.RouteDigest))); err != nil {
			return err
		}
	}
	for _, task := range data.Tasks {
		if _, err := fmt.Fprintf(writer, "Task %s: %s evidence=%s reason=%s\n",
			safeOverviewValue(task.TaskID), safeOverviewValue(task.TaskStatus),
			task.CompletionEvidence, valueOrUnavailable(task.Reason)); err != nil {
			return err
		}
	}
	return nil
}
