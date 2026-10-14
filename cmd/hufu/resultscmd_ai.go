package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/utils"
)

func runResultsQuery(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, request resultquery.Request, rows []inspectpkg.HistoricalResult, response resultsEnvelope) error {
	if options.preview {
		response.Operation = "preview"
		response.Model = options.model
		return writeResults(command.OutOrStdout(), options.output, response, false)
	}
	request.Question = utils.RedactSecrets(request.Question)
	snapshot := &operatorpkg.OperatorSnapshot{Scope: operatorpkg.ResolvedScope{TeamName: options.team}}
	if len(response.Coverage) > 0 {
		snapshot.Scope.WorkspaceExact = response.Coverage[0].Scope.Workspace
		if len(response.Coverage) == 1 && snapshot.Scope.TeamName == "" {
			snapshot.Scope.TeamName = response.Coverage[0].Scope.Team
		}
	}
	projectDir, err := deps.workspace.getwd()
	if err != nil {
		return resultsExit(2, err)
	}
	snapshot.Scope.ProjectDir = projectDir
	ctx, cancel := context.WithTimeout(command.Context(), 60*time.Second)
	defer cancel()
	generate := func(ctx context.Context, purpose, prompt string, profile sidecar.Profile) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Validate bounded input before starting provider/proxy processes.
		if len([]rune(prompt)) > profile.InputRuneLimit() {
			return "", fmt.Errorf("historical query evidence exceeds the model input budget")
		}
		generated, err := deps.generate(ctx, snapshot, options.model, purpose, prompt, profile)
		response.Model = generated.model
		if generated.usage != nil {
			response.Usage.InputTokens += generated.usage.InputTokens
			response.Usage.OutputTokens += generated.usage.OutputTokens
			response.Usage.TotalTokens += generated.usage.TotalTokens
			response.Usage.Available = true
		}
		return generated.analysis, err
	}
	result, queryErr := resultquery.Query(ctx, request, rows, generate)
	response.Query, response.Matched = &result, result.Matched
	response.Results = slices.Clone(result.Sources)
	if queryErr != nil {
		response.Operation, response.Error = "failed", safeResultsText(utils.RedactSecrets(queryErr.Error()))
		response.Results = slices.Clone(result.Candidates)
		if err := writeResults(command.OutOrStdout(), options.output, response, false); err != nil {
			return err
		}
		code := 3
		if result.Plan.Resolution == "needs_clarification" || result.Plan.Resolution == "unsupported" {
			code = 2
		}
		return resultsExit(code, fmt.Errorf("analysis failed: %s; offline results list/search/show remain available", response.Error))
	}
	if options.output == "json" {
		return writeResults(command.OutOrStdout(), options.output, response, false)
	}
	if err := writeResults(command.OutOrStdout(), options.output, response, false); err != nil {
		return err
	}
	if len(result.Sources) == 0 {
		_, err := fmt.Fprintln(command.OutOrStdout(), "No matching result was selected in this scope.")
		return err
	}
	if result.Truncated {
		if _, err := fmt.Fprintln(command.OutOrStdout(), "Coverage: selected evidence is sampled or truncated; this answer does not cover every result."); err != nil {
			return err
		}
	}
	if result.RecencyFallback {
		if _, err := fmt.Fprintln(command.OutOrStdout(), "Retrieval: no literal matches; the model reviewed recent results within the same scope."); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(command.OutOrStdout(), "\nLLM answer (%s; source-grounded inference):\n", safeResultsText(response.Model)); err != nil {
		return err
	}
	for _, block := range result.Blocks {
		citations := ""
		for _, id := range block.Sources {
			index := slices.IndexFunc(result.Sources, func(row inspectpkg.HistoricalResult) bool { return row.ResultID == id })
			citations += fmt.Sprintf(" [%d]", index+1)
		}
		if _, err := fmt.Fprintf(command.OutOrStdout(), "%s: %s%s\n", block.Kind, block.Text, citations); err != nil {
			return err
		}
	}
	for index, row := range result.Sources {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "[%d] %s — hufu results show %s%s\n", index+1, safeResultsText(row.Description), row.ResultID, resultsTeamArgument(row.Team)); err != nil {
			return err
		}
	}
	return nil
}
