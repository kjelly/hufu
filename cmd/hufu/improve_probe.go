package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/improver"
	"github.com/kjelly/hufu/internal/team"
)

type probeExitError struct{ cause error }

func (e *probeExitError) Error() string        { return e.cause.Error() }
func (e *probeExitError) Unwrap() error        { return e.cause }
func (e *probeExitError) ProcessExitCode() int { return 2 }

func newImproveProbeCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "probe", Short: "Run controlled, paired harness experiments",
		SilenceUsage: true, SilenceErrors: true,
	}
	command.AddCommand(newProbePlanCommand(), newProbeRunCommand(), newProbeResumeCommand(), newProbeShowCommand(), newProbeExportCommand())
	return command
}

func newProbePlanCommand() *cobra.Command {
	var configPath, outputPath string
	var asJSON bool
	command := &cobra.Command{
		Use: "plan", Short: "Validate and preview a frozen experiment without a provider or workspace",
		// Root presentation/timeout setup reads host provider configuration.
		// A static preview needs neither that configuration nor credentials.
		PersistentPreRun: func(_ *cobra.Command, _ []string) {},
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return &probeExitError{fmt.Errorf("plan takes no positional arguments")}
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			if configPath == "" {
				return &probeExitError{fmt.Errorf("--config is required")}
			}
			build, err := improver.CurrentBuildIdentity()
			if err != nil {
				return &probeExitError{err}
			}
			prepared, err := improver.Prepare(configPath, build)
			if err != nil {
				return &probeExitError{err}
			}
			raw, err := paired.CanonicalJSON(prepared.Preview)
			if err != nil {
				return &probeExitError{err}
			}
			if outputPath != "" {
				if err := team.AtomicCreateFile(outputPath, append(raw, '\n'), 0o600); err != nil {
					return &probeExitError{err}
				}
			}
			if asJSON {
				_, err = fmt.Fprintln(command.OutOrStdout(), string(raw))
			} else {
				p := prepared.Preview
				_, err = fmt.Fprintf(command.OutOrStdout(),
					"Experiment: %s\nMode: %s (%s)\nPlanned executor slots: %d\nAllowed surfaces: %d\nRuntime preflight: pending\nExecutable: false\n",
					p.ExperimentID, p.Mode, p.ExecutionMode, len(p.Slots), len(p.Surfaces))
			}
			return err
		},
	}
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &probeExitError{err} })
	command.Flags().StringVar(&configPath, "config", "", "Experiment YAML configuration")
	command.Flags().StringVar(&outputPath, "out", "", "Atomically create a new JSON preview file")
	command.Flags().BoolVar(&asJSON, "json", false, "Write only the JSON preview to stdout")
	return command
}

func init() {
	improveCmd.AddCommand(newImproveProbeCommand())
	improveCmd.AddCommand(newProbeExecutionCommand("refine"))
}
