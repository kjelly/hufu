package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/utils"
)

// selectFinalResults follows canonical run identity, never a task description,
// agent name, domain-specific payload reference, or the most recent worker.
func selectFinalResults(all []inspectpkg.HistoricalResult, request resultquery.Request, locator string) ([]inspectpkg.HistoricalResult, error) {
	var anchor *inspectpkg.HistoricalResult
	if locator != "" {
		found := findHistoricalResult(all, request, locator)
		if len(found) == 0 {
			return nil, fmt.Errorf("result %q was not found in the selected team/branch scope", locator)
		}
		if len(found) != 1 {
			return nil, errors.New("result ID prefix is ambiguous; use the full result-id")
		}
		anchor = &found[0]
		if anchor.Kind == "run_response" {
			return found, nil
		}
	}
	request.IncludeRunResponses = true
	rows := resultquery.Retrieve(all, resultquery.Plan{StatusBasis: "at_terminal", Order: "newest"}, request)
	rows = slices.DeleteFunc(rows, func(row inspectpkg.HistoricalResult) bool {
		if row.Kind != "run_response" {
			return true
		}
		if anchor == nil {
			return false
		}
		return row.Workspace != anchor.Workspace || row.Team != anchor.Team || row.SessionID != anchor.SessionID || row.RunID != anchor.RunID ||
			!slices.ContainsFunc(row.Branches, func(branch string) bool { return slices.Contains(anchor.Branches, branch) })
	})
	if anchor != nil {
		if len(rows) == 0 {
			return nil, errors.New("no saved final response for this result's run in the selected scope; the worker result is available, but the run may not have finished")
		}
		if len(rows) != 1 {
			return nil, errors.New("this result has different final responses across branches; select one with --branch")
		}
	}
	return rows, nil
}

func writeResultKind(writer io.Writer, row inspectpkg.HistoricalResult, coverage []inspectpkg.HistoricalCoverage) error {
	if row.Kind == "run_response" {
		_, err := fmt.Fprintf(writer, "Kind: saved final run response\nRun: %s\n", safeResultsText(row.RunID))
		return err
	}
	if _, err := fmt.Fprintf(writer, "Kind: individual agent task result (not the final run response)\nTask: %s · run: %s\n", safeResultsText(row.TaskID), safeResultsText(row.RunID)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(writer, "\nRead this run's complete final response:\n  %s\n\n", safeResultsText(resultsScopedCommand("final", row, coverage)))
	return err
}

func resultsShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// Reprint the saved final response body, without history metadata, previews,
// worker output, or model synthesis. Terminal controls remain display-safe.
func writeFinalResponse(writer io.Writer, format string, response resultsEnvelope) error {
	rows := response.Results
	if len(rows) != 1 || strings.TrimSpace(rows[0].Output) == "" {
		return resultsExit(2, errors.New("no saved final response content in this scope; use results runs --all-branches to browse older runs, or results list for individual tasks"))
	}
	if format == "json" {
		return writeResults(writer, format, response, true)
	}
	_, err := fmt.Fprintln(writer, utils.SanitizeTerminalText(utils.RedactSecrets(rows[0].Output)))
	return err
}
