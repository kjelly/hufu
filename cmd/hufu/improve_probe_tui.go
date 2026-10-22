package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/improver"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

func validateProbeDisplay(command *cobra.Command, asJSON, asTUI bool) error {
	if !asTUI {
		return nil
	}
	if asJSON {
		return &probeExitError{errors.New("--tui and --json are mutually exclusive")}
	}
	input, inputOK := command.InOrStdin().(*os.File)
	output, outputOK := command.ErrOrStderr().(*os.File)
	if !inputOK || !outputOK || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
		return &probeExitError{errors.New("--tui requires terminal stdin and stderr")}
	}
	return nil
}

func runProbeDisplay(command *cobra.Command, experiment *improver.Experiment, build improver.BuildIdentity, settings improver.RuntimeSettings, asTUI bool) (improver.ExperimentReport, error) {
	if !asTUI {
		return experiment.Run(command.Context(), build, settings, probeProgress(command.ErrOrStderr()))
	}
	ctx, cancel := context.WithCancel(command.Context())
	defer cancel()
	// Resolve presentation without loading provider/host configuration in a
	// fixture run. Existing TUI styles own adaptive palettes and redraw policy.
	theme := tuipkg.ThemeAuto
	var err error
	if opts.themeMode != "" {
		theme, err = tuipkg.ParseThemeMode(opts.themeMode)
		if err != nil {
			return improver.ExperimentReport{}, err
		}
	}
	preset := tuipkg.DisplayDefault
	if opts.displayPreset != "" {
		preset, err = tuipkg.ParseDisplayPreset(opts.displayPreset)
		if err != nil {
			return improver.ExperimentReport{}, err
		}
	}
	model := tuipkg.NewWithOptions("Experiment "+experiment.Plan.ExperimentID,
		tuipkg.TeamInfo{TeamName: "Controlled experiment"}, tuipkg.Options{
			Theme: theme, DisplayPreset: preset, Owner: true, Context: ctx,
			NoColor: opts.noColorMode || os.Getenv("NO_COLOR") != "", Spinner: !opts.noSpinner,
		})
	options := []tea.ProgramOption{tea.WithInput(command.InOrStdin()), tea.WithOutput(command.ErrOrStderr()),
		tea.WithContext(command.Context())}
	if preset == tuipkg.DisplayEpaper {
		options = append(options, tea.WithFPS(1))
	}
	program := tea.NewProgram(model, options...)
	deactivate := activateTUIProgram(program)
	defer deactivate()
	type completion struct {
		report improver.ExperimentReport
		err    error
	}
	done := make(chan completion, 1)
	workFinished := make(chan struct{})
	controlsDone := make(chan struct{})
	go func() {
		defer close(controlsDone)
		for {
			select {
			case <-model.WrapUpCh:
				cancel()
			case <-model.ReportCh:
				program.Send(tuipkg.StatusBarMsg{Text: "The verified report is saved when the experiment settles."})
			case <-workFinished:
				return
			}
		}
	}()
	go func() {
		report, runErr := experiment.Run(ctx, build, settings, func(state improver.LedgerState) {
			program.Send(tuipkg.TasksUpdatedMsg{Items: improver.StageTasks(state)})
			program.Send(tuipkg.StatusBarMsg{Text: fmt.Sprintf("spent=%d reserved=%d unknown=%d",
				state.Budget.SpentTokens, state.Budget.ReservedTokens, state.Budget.UnknownStages)})
		})
		var summary bytes.Buffer
		if report.Kind != "" {
			_ = writeProbeReport(&summary, report, false)
		} else if runErr != nil {
			summary.WriteString(runErr.Error())
		}
		program.Send(tuipkg.ResultMsg{Text: summary.String()})
		// No parent RunResult is fabricated from the experiment decision.
		program.Send(tuipkg.FinishedMsg{})
		program.Send(tuipkg.StatusBarMsg{Text: "Experiment: " + report.Decision})
		close(workFinished)
		done <- completion{report, runErr}
	}()
	_, viewErr := program.Run()
	cancel()
	// Wait for runtime drain before callers close the journal or workspace
	// lease. The executor's own deadline bounds work; quitting the UI cannot
	// authorize a replacement child while the old child is still active.
	result := <-done
	<-controlsDone
	return result.report, errors.Join(result.err, viewErr)
}
