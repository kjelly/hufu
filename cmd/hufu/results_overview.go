package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

// The default view presents saved run responses, not internal worker attempts.
// Previews are literal excerpts, never inferred summaries or acceptance claims.
func writeResultsOverview(writer io.Writer, response resultsEnvelope, options *resultsCLIOptions) error {
	if _, err := fmt.Fprintf(writer, "Saved final responses: %d (team: %s; showing %d)\n", response.Matched, cmpTeamLabel(response.Team), len(response.Results)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, "Each entry is a saved final run response. Individual agent tasks: hufu results list"+resultsTeamArgument(response.Team)); err != nil {
		return err
	}
	if options.allBranches {
		if _, err := fmt.Fprintln(writer, "Scope: all saved branches and sessions."); err != nil {
			return err
		}
	}
	for _, coverage := range response.Coverage {
		if !coverage.Scope.AllBranches {
			if _, err := fmt.Fprintln(writer, "Scope: selected branch history. Include older sessions and other branches with --all-branches."); err != nil {
				return err
			}
			break
		}
	}
	if response.Matched == 0 {
		_, err := fmt.Fprintln(writer, "No saved final responses in this scope. Individual agent results may exist while a run is unfinished.")
		return err
	}
	for index, row := range response.Results {
		if _, err := fmt.Fprintf(writer, "\n%d. %s · %s · %s\nPreview (excerpt):\n", index+1, row.CompletedAt.Format(time.RFC3339), safeResultsText(row.Team), safeResultsText(row.Status)); err != nil {
			return err
		}
		if err := writeResultPreview(writer, row.Output); err != nil {
			return err
		}
		command := "hufu results show " + row.ResultID + resultsTeamArgument(row.Team)
		if options.workspace != "" {
			command += " --workspace " + resultsShellQuote(options.workspace)
		}
		if options.allBranches {
			command += " --all-branches"
		} else if options.branch != "" {
			command += " --branch " + resultsShellQuote(options.branch)
		}
		if _, err := fmt.Fprintln(writer, "Read full response: "+safeResultsText(command)); err != nil {
			return err
		}
	}
	if response.NextCursor != "" {
		_, err := fmt.Fprintf(writer, "\nNext page: --cursor %s\n", response.NextCursor)
		return err
	}
	return nil
}

func writeResultPreview(writer io.Writer, output string) error {
	shown := 0
	for line := range strings.SplitSeq(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if shown == 3 {
			break
		}
		if _, err := fmt.Fprintln(writer, "  "+operatorpkg.SafeDisplayText(line, 140)); err != nil {
			return err
		}
		shown++
	}
	if shown == 0 {
		_, err := fmt.Fprintln(writer, "  (No response content saved.)")
		return err
	}
	return nil
}

func resultsScopedCommand(operation string, row inspectpkg.HistoricalResult, coverage []inspectpkg.HistoricalCoverage) string {
	command := "hufu results " + operation + " " + row.ResultID + resultsTeamArgument(row.Team)
	for _, scope := range coverage {
		if scope.Scope.Workspace != row.Workspace {
			continue
		}
		command += " --workspace " + resultsShellQuote(scope.Scope.Workspace)
		if scope.Scope.AllBranches {
			command += " --all-branches"
		} else if scope.Scope.Branch != "" {
			command += " --branch " + resultsShellQuote(scope.Scope.Branch)
		}
		break
	}
	return command
}
