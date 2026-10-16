package main

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	tuipkg "github.com/kjelly/hufu/internal/tui"
	"github.com/kjelly/hufu/internal/utils"
)

func validateResultsPresentation(options *resultsCLIOptions, operation string) error {
	if operation == "pick" && options.displayMode == "plain" {
		return errors.New("pick requires an interactive terminal display; use runs/list/search with --display-mode plain")
	}
	if options.theme != "" {
		if _, err := tuipkg.ParseThemeMode(options.theme); err != nil {
			return err
		}
	}
	if options.displayPreset != "" {
		if _, err := tuipkg.ParseDisplayPreset(options.displayPreset); err != nil {
			return err
		}
	}
	if options.displayMode != "" && options.displayMode != "auto" && options.displayMode != "terminal" && options.displayMode != "plain" {
		return errors.New("--display-mode must be auto, terminal, or plain")
	}
	if options.render != "" && options.render != "plain" && options.render != "markdown" {
		return errors.New("--render must be plain or markdown")
	}
	if options.payload && (operation != "show" || options.output != "text" || options.render == "markdown") {
		return errors.New("show --payload prints a JSON value; use it without --output json or --render markdown")
	}
	if options.render == "markdown" && (options.output != "text" || !resultsReadFull(operation) && operation != "pick") {
		return errors.New("--render markdown applies to text read/final/show/latest results")
	}
	return nil
}

func presentResults(command *cobra.Command, options *resultsCLIOptions, operation string, response resultsEnvelope, full bool) error {
	if options.payload {
		return writeResultsPayload(command.OutOrStdout(), response)
	}
	var body bytes.Buffer
	var err error
	switch {
	case operation == "report" && options.output == "text":
		_, err = body.WriteString(utils.SanitizeTerminalText(response.ReportMarkdown))
	case operation == "overview" && options.output == "text":
		err = writeResultsOverview(&body, response, options)
	case operation == "read" || operation == "final":
		err = writeFinalResponse(&body, options.output, response)
	default:
		err = writeResults(&body, options.output, response, full)
	}
	if err != nil {
		return err
	}
	content := body.String()
	if options.render == "markdown" {
		content, err = renderResultsMarkdown(command.OutOrStdout(), options, content)
		if err != nil {
			return err
		}
	}
	if full && options.output == "text" && !options.quiet {
		if err := writeResultsSource(command.ErrOrStderr(), response); err != nil {
			return err
		}
	}
	_, err = io.WriteString(command.OutOrStdout(), content)
	return err
}

func writeResultsSource(writer io.Writer, response resultsEnvelope) error {
	if len(response.Results) != 1 {
		return nil
	}
	row := response.Results[0]
	_, err := fmt.Fprintf(writer, "Saved result · team=%s · completed=%s\nInvocation: %s\nRun: %s\nResult: %s · source event: %s\n",
		cmpTeamLabel(row.Team), row.CompletedAt.UTC().Format(time.RFC3339), safeResultsText(row.InvocationID),
		safeResultsText(row.RunID), safeResultsText(row.ResultID), safeResultsText(row.SourceEventID))
	return err
}

// Payload-only output never uses the model context preview, decodes numbers to
// float64, or follows artifact paths. Scope and redaction are owned by the loader.
func writeResultsPayload(writer io.Writer, response resultsEnvelope) error {
	if len(response.Results) != 1 || response.Results[0].TypedResult == nil || response.Results[0].TypedResult.StructuredPayload == nil {
		return resultsExit(2, errors.New("this result has no structured payload; read its text or use --output json for the complete result envelope"))
	}
	raw := response.Results[0].TypedResult.StructuredPayload.Value
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return resultsExit(2, errors.New("this result has no structured payload value"))
	}
	formatted, err := jsontext.AppendFormat(nil, raw, jsontext.WithIndent("  "))
	if err != nil {
		return resultsExit(2, fmt.Errorf("invalid saved structured payload: %w", err))
	}
	_, err = fmt.Fprintln(writer, string(formatted))
	return err
}

func resultsMarkdownEnabled(terminal, noColor bool, noColorEnv, displayMode, preset string, theme tuipkg.ThemeMode) bool {
	return terminal && !noColor && noColorEnv == "" && displayMode != "plain" && preset != "epaper" && theme != tuipkg.ThemeMono
}

func renderResultsMarkdown(writer io.Writer, options *resultsCLIOptions, content string) (string, error) {
	file, terminal := writer.(*os.File)
	terminal = terminal && term.IsTerminal(file.Fd())
	presentation := config.LoadConfig().Presentation
	themeValue := firstNonEmpty(options.theme, presentation.Theme, "auto")
	theme, err := tuipkg.ParseThemeMode(themeValue)
	if err != nil {
		return "", err
	}
	if !resultsMarkdownEnabled(terminal, options.noColor, os.Getenv("NO_COLOR"), options.displayMode,
		firstNonEmpty(options.displayPreset, presentation.DisplayPreset), theme) {
		return content, nil
	}
	profile := colorprofile.Detect(file, os.Environ())
	if profile <= colorprofile.ASCII {
		return content, nil
	}
	width, _, err := term.GetSize(file.Fd())
	if err != nil {
		width = 80
	}
	rendered, err := formatResultsMarkdown(content, width, tuipkg.ResolveThemeMode(theme))
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	profileWriter := colorprofile.Writer{Forward: &output, Profile: profile}
	if _, err := io.WriteString(&profileWriter, rendered); err != nil {
		return "", err
	}
	return output.String(), nil
}

func formatResultsMarkdown(content string, width int, theme tuipkg.ThemeMode) (string, error) {
	// Rendering is optional; large reports remain readable with --render plain.
	if len(content) > 1<<20 {
		return "", resultsExit(2, errors.New("markdown rendering exceeds the 1 MiB budget; use --render plain to read the complete result"))
	}
	renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(string(theme)),
		glamour.WithWordWrap(max(20, min(width, 240))), glamour.WithPreservedNewLines())
	if err != nil {
		return "", err
	}
	return renderer.Render(utils.SanitizeTerminalText(utils.RedactSecrets(content)))
}
