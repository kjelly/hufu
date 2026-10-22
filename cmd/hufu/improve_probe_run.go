package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/improver"
	"github.com/kjelly/hufu/internal/scripted"
)

type probeOutcomeError struct {
	cause error
	code  int
}

func (e *probeOutcomeError) Error() string        { return e.cause.Error() }
func (e *probeOutcomeError) Unwrap() error        { return e.cause }
func (e *probeOutcomeError) ProcessExitCode() int { return e.code }

func probeRuntimeSettings(mode string) improver.RuntimeSettings {
	if mode == "fixture" {
		return improver.RuntimeSettings{ProviderURL: scripted.Endpoint}
	}
	cfg := config.LoadConfig()
	return improver.RuntimeSettings{ProviderURL: cfg.ProviderURL, ProviderAPIKey: cfg.ProviderAPIKey, Providers: cfg.Providers}
}

func configureProbeOperation(command *cobra.Command) {
	command.PersistentPreRun = func(_ *cobra.Command, _ []string) {}
	command.Args = func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return &probeExitError{fmt.Errorf("%s takes no positional arguments", command.Name())}
		}
		return nil
	}
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &probeExitError{err} })
}

func validateProbeOverrides(command *cobra.Command) error {
	for _, name := range []string{
		"model", "worker-model", "timeout", "max-steps", "max-rounds", "max-concurrent",
		"max-duration", "max-total-tokens", "profile", "provider-url", "provider-api-key",
		"sidecar-model", "guard-model", "judge-model", "plan-reviewer-model", "temperature",
		"max-tokens", "context-window", "top-p", "top-k", "var", "var-file", "skill",
		"plan", "auto-skills", "force-mcp", "helper-tools", "dry-run",
	} {
		if flag := command.Flag(name); flag != nil && flag.Changed {
			return &probeExitError{fmt.Errorf("--%s cannot override a frozen probe; use the experiment configuration", name)}
		}
	}
	return nil
}

func newProbeRunCommand() *cobra.Command {
	return newProbeExecutionCommand("manual")
}

func newProbeExecutionCommand(mode string) *cobra.Command {
	var configPath, path string
	var asJSON, asTUI bool
	command := &cobra.Command{Use: "run", Short: "Freeze and execute a manual paired experiment"}
	if mode == "refine" {
		command.Use, command.Short = "refine", "Run one bounded propose, probe, compose and reflect schedule"
		command.SilenceUsage, command.SilenceErrors = true, true
	}
	configureProbeOperation(command)
	command.RunE = func(command *cobra.Command, _ []string) (retErr error) {
		if configPath == "" || path == "" {
			return &probeExitError{errors.New("--config and --workspace are required")}
		}
		if err := validateProbeOverrides(command); err != nil {
			return err
		}
		if err := validateProbeDisplay(command, asJSON, asTUI); err != nil {
			return err
		}
		build, err := improver.CurrentBuildIdentity()
		if err != nil {
			return &probeExitError{err}
		}
		prepared, err := improver.Prepare(configPath, build)
		if err != nil {
			return &probeExitError{err}
		}
		if prepared.Config.Mode != mode {
			return &probeExitError{fmt.Errorf("%s requires mode: %s", command.Name(), mode)}
		}
		settings := probeRuntimeSettings(prepared.Config.ExecutionMode)
		workspace, err := improver.OpenProbeWorkspace(path, false)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, workspace.Close()) }()
		experiment, err := improver.CreateExperiment(command.Context(), workspace, prepared, settings)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, experiment.Close()) }()
		report, runErr := runProbeDisplay(command, experiment, build, settings, asTUI)
		return finishProbeOutput(command, report, asJSON, runErr)
	}
	command.Flags().StringVar(&configPath, "config", "", "Experiment YAML configuration")
	command.Flags().StringVar(&path, "workspace", "", "Explicit private control workspace")
	command.Flags().BoolVar(&asJSON, "json", false, "Write only the JSON report to stdout")
	command.Flags().BoolVar(&asTUI, "tui", false, "Show canonical stage progress in the existing task dashboard")
	return command
}

func newProbeResumeCommand() *cobra.Command {
	var id, path string
	var asJSON, asTUI bool
	command := &cobra.Command{Use: "resume", Short: "Reconcile and continue an already frozen experiment"}
	configureProbeOperation(command)
	command.RunE = func(command *cobra.Command, _ []string) (retErr error) {
		if !paired.ValidID(id) || path == "" {
			return &probeExitError{errors.New("--experiment and --workspace are required")}
		}
		if err := validateProbeOverrides(command); err != nil {
			return err
		}
		if err := validateProbeDisplay(command, asJSON, asTUI); err != nil {
			return err
		}
		build, err := improver.CurrentBuildIdentity()
		if err != nil {
			return &probeExitError{err}
		}
		workspace, err := improver.OpenProbeWorkspace(path, false)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, workspace.Close()) }()
		experiment, err := improver.OpenExperiment(command.Context(), workspace, id)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, experiment.Close()) }()
		settings := probeRuntimeSettings(experiment.Plan.ExecutionMode)
		report, runErr := runProbeDisplay(command, experiment, build, settings, asTUI)
		return finishProbeOutput(command, report, asJSON, runErr)
	}
	command.Flags().StringVar(&id, "experiment", "", "Frozen experiment ID")
	command.Flags().StringVar(&path, "workspace", "", "Existing private control workspace")
	command.Flags().BoolVar(&asJSON, "json", false, "Write only the JSON report to stdout")
	command.Flags().BoolVar(&asTUI, "tui", false, "Show canonical stage progress in the existing task dashboard")
	return command
}

func newProbeShowCommand() *cobra.Command {
	var id, path string
	var asJSON bool
	command := &cobra.Command{Use: "show", Short: "Verify and read an experiment without providers or workspace writes"}
	configureProbeOperation(command)
	command.RunE = func(command *cobra.Command, _ []string) (retErr error) {
		if !paired.ValidID(id) || path == "" {
			return &probeExitError{errors.New("--experiment and --workspace are required")}
		}
		workspace, err := improver.OpenProbeWorkspace(path, true)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, workspace.Close()) }()
		experiment, err := improver.OpenExperiment(command.Context(), workspace, id)
		if err != nil {
			return &probeExitError{err}
		}
		defer func() { retErr = errors.Join(retErr, experiment.Close()) }()
		report, err := experiment.Report(command.Context())
		if err != nil {
			return &probeExitError{err}
		}
		// Reading a valid historical failure is a successful read operation.
		return writeProbeReport(command.OutOrStdout(), report, asJSON)
	}
	command.Flags().StringVar(&id, "experiment", "", "Frozen experiment ID")
	command.Flags().StringVar(&path, "workspace", "", "Existing private control workspace")
	command.Flags().BoolVar(&asJSON, "json", false, "Write only the JSON report to stdout")
	return command
}

func probeProgress(writer io.Writer) func(improver.LedgerState) {
	last := ""
	return func(state improver.LedgerState) {
		text := fmt.Sprintf("Experiment %s: probes=%d spent=%d reserved=%d unknown=%d\n",
			state.Scope.ExperimentID, state.ProbeRuns, state.Budget.SpentTokens, state.Budget.ReservedTokens, state.Budget.UnknownStages)
		if text != last {
			_, _ = io.WriteString(writer, text)
			last = text
		}
	}
}

func writeProbeReport(writer io.Writer, report improver.ExperimentReport, asJSON bool) error {
	if asJSON {
		raw, err := paired.CanonicalJSON(report)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(writer, string(raw))
		return err
	}
	_, err := fmt.Fprintf(writer, "Experiment: %s\nMode: %s\nDecision: %s\nSelected variant: %s\nScored: %d/%d\nTokens: %d spent, %d reserved; %d unknown stages\n",
		report.Scope.ExperimentID, report.ExecutionMode, report.Decision, report.SelectedVariant,
		report.Coverage.Scored, report.Coverage.Planned, report.Budget.SpentTokens, report.Budget.ReservedTokens, report.Budget.UnknownStages)
	if err != nil {
		return err
	}
	if report.SimulationDecision != "" {
		_, err = fmt.Fprintf(writer, "Simulated decision: %s (synthetic fixture evidence)\n", report.SimulationDecision)
	}
	return err
}

func finishProbeOutput(command *cobra.Command, report improver.ExperimentReport, asJSON bool, runErr error) error {
	if report.Kind != "" {
		if err := writeProbeReport(command.OutOrStdout(), report, asJSON); err != nil {
			return &probeExitError{errors.Join(runErr, err)}
		}
	}
	if errors.Is(runErr, context.Canceled) || report.Decision == "cancelled" {
		return &probeOutcomeError{cause: errors.Join(runErr, errors.New("experiment cancelled")), code: 130}
	}
	if runErr != nil {
		return &probeExitError{runErr}
	}
	switch report.Decision {
	case "eligible_for_review", "no_change", "simulation_only":
		return nil
	case "rejected":
		return &probeOutcomeError{cause: errors.New("experiment rejected"), code: 1}
	default:
		return &probeExitError{fmt.Errorf("experiment %s", report.Decision)}
	}
}
