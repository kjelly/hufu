package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

func renderAcceptedDeliverable(_ context.Context, row inspectpkg.HistoricalResult) (string, bool, error) {
	registry := team.NewTeamRegistry(resolveSearchPaths())
	if err := registry.Discover(); err != nil {
		return "", false, err
	}
	dir, err := registry.Resolve(row.Team)
	if err != nil {
		return "", false, fmt.Errorf("locate the original team schema: %w", err)
	}
	return team.RenderAcceptedResultReport(dir, row.TypedResult)
}

func presentAcceptedDeliverable(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, request resultquery.Request, all, runs []inspectpkg.HistoricalResult, response resultsEnvelope, exactRun bool) error {
	if len(runs) == 0 {
		return resultsExit(2, errors.New("no saved run in this scope; select a run with --run or a result ID"))
	}
	render := deps.deliverable
	if render == nil {
		render = renderAcceptedDeliverable
	}
	var selected *inspectpkg.HistoricalResult
	var content, sourceStatus string
	tasks := resultquery.Retrieve(all, resultquery.Plan{StatusBasis: "at_terminal", Order: "newest"}, request)
	for _, run := range runs {
		for _, row := range tasks {
			if row.Kind != "task" || row.Workspace != run.Workspace || row.Team != run.Team || row.SessionID != run.SessionID || row.RunID != run.RunID ||
				!slices.ContainsFunc(row.Branches, func(branch string) bool { return slices.Contains(run.Branches, branch) }) {
				continue
			}
			if row.Validity != "accepted" || row.CurrentStatus != "done" || row.TypedResult == nil || row.TypedResult.StructuredPayload == nil {
				continue
			}
			text, declared, err := render(command.Context(), row)
			if err != nil {
				return resultsExit(2, fmt.Errorf("render accepted result %s: %w", row.ResultID, err))
			}
			if !declared {
				continue
			}
			if selected != nil {
				return resultsExit(2, errors.New("multiple accepted report tasks in the selected run; report ownership is ambiguous"))
			}
			if strings.TrimSpace(text) == "" {
				return resultsExit(2, errors.New("accepted report template rendered empty output"))
			}
			selected, content = &row, text
		}
		if selected != nil {
			sourceStatus = firstNonEmpty(run.Status, "unknown")
			break
		}
		if exactRun {
			break
		}
	}
	if selected == nil {
		return resultsExit(2, errors.New("no accepted task declaring x-hufu-final-report in the selected scope; use results show --payload to inspect individual results"))
	}
	content = utils.SanitizeTerminalText(utils.RedactSecrets(content))
	response.Results = []inspectpkg.HistoricalResult{*selected}
	response.Matched, response.Deliverable, response.SourceRunStatus = 1, content, sourceStatus
	if options.exportPath != "" {
		if err := exportRunReport(options.exportPath, content); err != nil {
			return resultsExit(2, err)
		}
	}
	return presentResults(command, options, "deliverable", response, true)
}
