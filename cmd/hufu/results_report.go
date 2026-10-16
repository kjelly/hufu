package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
	"github.com/spf13/cobra"
)

func reportDataFromSaved(saved *team.RunReport) *reportData {
	data := &reportData{
		ReportSource: saved, StartedAt: saved.StartedAt, CompletedAt: saved.CompletedAt,
		RunResult: saved.Result, Todos: saved.Tasks, InvocationID: saved.InvocationID, SourceRunID: saved.RunID,
		SessionData: &team.SessionData{RequiredResourceLockSet: saved.RequiredResourceLockSet}, DecisionPrimitives: saved.DecisionPrimitives,
		AuditUnavailableReason: "live artifact verification is not part of this immutable historical snapshot",
		CostUnavailableReason:  saved.CostUnavailableReason, Cost: saved.Cost, ModelProfiles: saved.ModelProfiles, ContextRouting: saved.ContextRouting, ControlDecisions: saved.ControlDecisions, DeprecatedMemory: saved.DeprecatedMemory,
	}
	data.ReviewScope = gatherRuntimeReviewScope(saved.Tasks)
	if !reviewScopeBoundToRunResult(data.ReviewScope, saved.Result) {
		data.ReviewScope = nil
	}
	if manifest := saved.Result.EvidenceManifest; manifest != nil {
		data.EvidenceIdentity = manifest.ManifestHash
	}
	return data
}

func renderSavedRunReport(saved *team.RunReport) string {
	return buildReportMD(reportDataFromSaved(saved), saved.Team, saved.Result.Response)
}

func presentRunReport(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, response resultsEnvelope) error {
	if len(response.Results) != 1 {
		return resultsExit(2, errors.New("no saved run response in the selected scope"))
	}
	row := response.Results[0]
	load := deps.report
	if load == nil {
		load = team.LoadRunReport
	}
	saved, err := load(command.Context(), row.Workspace, row.SourceEventID)
	if err != nil {
		code := 1
		if errors.Is(err, team.ErrRunReportUnavailable) {
			code = 2
		}
		return resultsExit(code, fmt.Errorf("report for run %s: %w", row.RunID, err))
	}
	if saved == nil || saved.Result == nil || saved.SourceEventID != row.SourceEventID || saved.SourceHash != row.SourceHash || saved.RunID != row.RunID || saved.SessionID != row.SessionID || saved.Team != row.Team || saved.InvocationID != row.InvocationID {
		return resultsExit(1, errors.New("saved report does not match the selected result"))
	}
	response.Report = saved
	response.ReportMarkdown = utils.RedactSecrets(renderSavedRunReport(saved))
	if options.exportPath != "" {
		if err := exportRunReport(options.exportPath, response.ReportMarkdown); err != nil {
			return resultsExit(2, err)
		}
	}
	return presentResults(command, options, "report", response, true)
}

func exportRunReport(path, content string) error {
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Explicit exports may replace files, but never a SQLite authority or its
	// sidecars, including databases in a different workspace.
	name := strings.ToLower(filepath.Base(target))
	if strings.HasPrefix(name, runtimestore.File) || strings.HasPrefix(name, "context.sqlite") {
		return fmt.Errorf("report export cannot replace a runtime or context database: %s", target)
	}
	return team.AtomicWriteFile(target, []byte(content), 0o600)
}
