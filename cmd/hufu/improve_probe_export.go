package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/improver"
)

func newProbeExportCommand() *cobra.Command {
	var id, workspacePath, destination string
	var asJSON bool
	command := &cobra.Command{Use: "export", Short: "Create a self-contained review bundle without applying changes"}
	configureProbeOperation(command)
	command.RunE = func(command *cobra.Command, _ []string) (retErr error) {
		if !paired.ValidID(id) || workspacePath == "" || destination == "" {
			return &probeExitError{errors.New("--experiment, --workspace and --out are required")}
		}
		workspace, err := improver.OpenProbeWorkspace(workspacePath, true)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, workspace.Close()) }()
		experiment, err := improver.OpenExperiment(command.Context(), workspace, id)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, experiment.Close()) }()
		bundle, err := experiment.Export(command.Context(), destination)
		if err != nil {
			return &probeExitError{err}
		}
		if asJSON {
			raw, err := paired.CanonicalJSON(bundle)
			if err != nil {
				return &probeExitError{err}
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), string(raw))
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Review bundle: %s\nDecision: %s\nSelected variant: %s\nSynthetic: %t\nLesson: %s\n",
			destination, bundle.Decision, bundle.SelectedVariant, bundle.Synthetic, bundle.Lesson.Status)
		return err
	}
	command.Flags().StringVar(&id, "experiment", "", "Finished experiment ID")
	command.Flags().StringVar(&workspacePath, "workspace", "", "Existing private control workspace")
	command.Flags().StringVar(&destination, "out", "", "New review directory; existing paths are refused")
	command.Flags().BoolVar(&asJSON, "json", false, "Write only the bundle manifest to stdout")
	return command
}
