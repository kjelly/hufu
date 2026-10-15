package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/belief"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func newInspectBeliefsCommand(options *inspectCLIOptions) *cobra.Command {
	var taskID string
	command := &cobra.Command{
		Use: "beliefs <run-id>", Short: "Rebuild Bayesian belief state and show its locked model and sources",
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
			result, inspectErr := inspectpkg.InspectBeliefs(command.Context(), query)
			return finishInspect(command, format, result, inspectErr)
		},
	}
	command.Flags().StringVar(&taskID, "task", "", "Optional exact task ID filter")
	return command
}

func renderInspectBeliefsText(writer io.Writer, query inspectpkg.InspectQuery, data inspectpkg.BeliefsData) error {
	if _, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\n", safeOverviewValue(data.RunID), safeOverviewValue(query.BranchID)); err != nil {
		return err
	}
	for _, execution := range data.Execution {
		if _, err := fmt.Fprintf(writer, "Execution %s: %s (attempt %d)\n", safeOverviewValue(execution.TaskID), safeOverviewValue(execution.Status), execution.Attempt); err != nil {
			return err
		}
	}
	if len(data.Models) == 0 {
		if _, err := fmt.Fprintln(writer, "Beliefs: not recorded"); err != nil {
			return err
		}
	}
	for _, model := range data.Models {
		synthetic := "unavailable"
		status := "available"
		if model.Unavailable != "" {
			status = "unavailable (" + model.Unavailable + ")"
		}
		if model.Synthetic != nil {
			synthetic = fmt.Sprint(*model.Synthetic)
		}
		if _, err := fmt.Fprintf(writer, "Model: %s version=%s synthetic=%s digest=%s experiment=%s status=%s\n", safeOverviewValue(valueOrUnavailable(model.ID)), safeOverviewValue(valueOrUnavailable(model.Version)), synthetic, safeOverviewValue(model.Digest), safeOverviewValue(model.ExperimentID), safeOverviewValue(status)); err != nil {
			return err
		}
	}
	for _, epoch := range data.Epochs {
		if _, err := fmt.Fprintf(writer, "Epoch %s: task=%s attempt=%d closed=%t\n", safeOverviewValue(epoch.Epoch.ID), safeOverviewValue(epoch.Epoch.Scope.TaskID), epoch.Epoch.Scope.Attempt, epoch.Closed); err != nil {
			return err
		}
		for _, source := range epoch.Sources {
			if _, err := fmt.Fprintf(writer, "  Source: event=%s hash=%s receipt=%s\n", safeOverviewValue(source.EventID), safeOverviewValue(source.EventHash), safeOverviewValue(source.ReceiptKey)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "  Retracted: %s\n", safeOverviewValue(refsOrNone(epoch.Retracted))); err != nil {
			return err
		}
		for _, observation := range epoch.Observations {
			if _, err := fmt.Fprintf(writer, "  Observation: %s experiment=%s outcome=%s source=%s\n", safeOverviewValue(observation.ID), safeOverviewValue(observation.ExperimentID), safeOverviewValue(observation.OutcomeID), safeOverviewValue(observation.SourceID)); err != nil {
				return err
			}
		}
		if epoch.State == nil {
			if _, err := fmt.Fprintf(writer, "  Belief: unavailable (%s)\n", safeOverviewValue(epoch.Unavailable)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(writer, "  Belief: %s digest=%s\n", epoch.State.Status, safeOverviewValue(epoch.State.Digest)); err != nil {
			return err
		}
		if epoch.State.Distribution == nil {
			if _, err := fmt.Fprintln(writer, "  Distribution: unavailable"); err != nil {
				return err
			}
		} else {
			for _, mass := range *epoch.State.Distribution {
				if _, err := fmt.Fprintf(writer, "  %s: %d.%09d\n", safeOverviewValue(mass.HypothesisID), mass.Units/belief.Scale, mass.Units%belief.Scale); err != nil {
					return err
				}
			}
		}
	}
	for _, diagnostic := range data.Diagnostics {
		if _, err := fmt.Fprintf(writer, "Historical diagnostic: %s source=%s\n", safeOverviewValue(diagnostic.Code), safeOverviewValue(valueOrUnavailable(diagnostic.SourceEventID))); err != nil {
			return err
		}
	}
	return nil
}
