package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

type explainCLIOptions struct {
	workspace string
	team      string
	run       string
	branch    string
	output    string
	json      bool
	ai        bool
	model     string
}

type explainTaskProgress struct {
	total     int
	done      int
	active    int
	waiting   int
	attention int
	skipped   int
}

func newExplainCommand() *cobra.Command {
	options := new(explainCLIOptions)
	command := &cobra.Command{
		Use:     "explain [question]",
		Aliases: []string{"explan"},
		Short:   "Explain the current progress of an active workspace",
		Long: `Explain reads Hufu's durable event projection and describes what the
selected workspace is doing, how its tasks are progressing, what needs
attention, and the safest next action. It is read-only and never starts,
resumes, retries, or changes a run.`,
		Example: `  hufu explain
  hufu explan --team hufu-code-review
  hufu explain --workspace ./workspace --run run-123
  hufu explain --ai "為什麼任務失敗" --model local/qwen3:8b
  hufu explain --json`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := resolveExplainOutput(command, options)
			if err != nil {
				return finishExplain(command, format, inspectpkg.InspectQuery{}, nil, fmt.Errorf("%w: %v", inspectpkg.ErrInvalidQuery, err))
			}
			if len(args) > 0 && !options.ai {
				return finishExplain(command, format, inspectpkg.InspectQuery{}, nil, fmt.Errorf("%w: an explanation prompt requires --ai", inspectpkg.ErrInvalidQuery))
			}
			if strings.TrimSpace(options.model) != "" && !options.ai {
				return finishExplain(command, format, inspectpkg.InspectQuery{}, nil, fmt.Errorf("%w: --model requires --ai", inspectpkg.ErrInvalidQuery))
			}
			query, err := explainQuery(command, options)
			if err != nil {
				return finishExplain(command, format, query, nil, err)
			}
			envelope, inspectErr := inspectpkg.InspectOverview(command.Context(), query)
			if inspectErr != nil || !options.ai {
				return finishExplain(command, format, query, envelope, inspectErr)
			}
			question := defaultExplainAIQuestion
			if len(args) > 0 {
				question = args[0]
			}
			return finishExplainAI(command, format, query, envelope, question, options.model)
		},
	}
	flags := command.Flags()
	flags.StringVarP(&options.workspace, "workspace", "w", "", "Workspace directory (default: active managed workspace)")
	flags.StringVar(&options.team, "team", "", "Team name when the project has multiple active managed workspaces")
	flags.StringVar(&options.run, "run", "", "Exact run ID (default: active binding or sole candidate)")
	flags.StringVar(&options.branch, "branch", "", "Exact branch ID, name, or label (default: active branch)")
	flags.StringVar(&options.output, "output", "text", "Output format: text or json")
	flags.BoolVar(&options.json, "json", false, "Alias for --output json")
	flags.BoolVar(&options.ai, "ai", false, "Ask an LLM to analyze the verified progress projection")
	flags.StringVarP(&options.model, "model", "m", "", "Model for --ai analysis (overrides workspace and config defaults)")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	return command
}

func finishExplainAI(command *cobra.Command, format string, query inspectpkg.InspectQuery, envelope *inspectpkg.Envelope, question, requestedModel string) error {
	if envelope == nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: overview is unavailable")}
	}
	overview, ok := envelope.Data.(inspectpkg.OverviewData)
	if !ok || overview.Snapshot == nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: overview snapshot is unavailable")}
	}
	analysisQuery := query
	analysisQuery.RunID = overview.Snapshot.Scope.RunID
	analysisQuery.BranchID = overview.Snapshot.Scope.BranchID
	analysisQuery.SessionID = overview.Snapshot.Scope.SessionID
	result, err := runExplainAI(command.Context(), analysisQuery, overview.Snapshot, question, requestedModel)
	if err != nil {
		command.Root().SilenceErrors = true
		command.Root().SilenceUsage = true
		safeErr := operatorpkg.SafeDisplayText(err.Error(), 1000)
		if format == "json" {
			failure := explainAIEnvelope{
				SchemaVersion: inspectpkg.SchemaVersion, Kind: "ai_explanation", Query: envelope.Query, Integrity: envelope.Integrity,
				Data: explainAIData{
					Operation: inspectpkg.OperationView{Status: "failed"}, Model: result.model, AnalysisIsInference: true,
					Overview: overview.Snapshot, Error: &inspectpkg.OperatorErrorView{Kind: "analysis", Code: "ai_analysis_failed", Message: safeErr},
				},
			}
			encoder := json.NewEncoder(command.OutOrStdout())
			encoder.SetEscapeHTML(false)
			if encodeErr := encoder.Encode(failure); encodeErr != nil {
				return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: encode AI failure: %w", encodeErr)}
			}
		} else {
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "hufu explain: %s\n", safeErr)
		}
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: %w", err)}
	}
	if format == "json" {
		success := explainAIEnvelope{
			SchemaVersion: inspectpkg.SchemaVersion, Kind: "ai_explanation", Query: envelope.Query, Integrity: envelope.Integrity,
			Data: explainAIData{
				Operation: inspectpkg.OperationView{Status: "succeeded"}, Model: result.model, Analysis: result.analysis,
				AnalysisIsInference: true, Overview: overview.Snapshot,
			},
		}
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(success); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: encode AI analysis: %w", err)}
		}
		return nil
	}
	if err := renderExplainText(command.OutOrStdout(), overview.Snapshot); err != nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: render text: %w", err)}
	}
	if _, err := fmt.Fprintf(command.OutOrStdout(), "\nAI analysis (%s; inference):\n%s\n", safeOverviewValue(result.model), result.analysis); err != nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: render AI analysis: %w", err)}
	}
	return nil
}

func resolveExplainOutput(command *cobra.Command, options *explainCLIOptions) (string, error) {
	format, err := resolveOutputAlias(
		"output", options.output, flagChanged(command, "output"),
		"json", outputForJSONAlias(options.json), flagChanged(command, "json"),
		"text", []string{"text", "json"},
	)
	if err != nil && options.json {
		return "json", err
	}
	return format, err
}

func explainQuery(command *cobra.Command, options *explainCLIOptions) (inspectpkg.InspectQuery, error) {
	workspace := strings.TrimSpace(options.workspace)
	if workspace == "" {
		resolved, err := resolveExistingManagedWorkspacePath(command.Context(), runtimeStartDir(), options.team)
		if err != nil {
			return inspectpkg.InspectQuery{}, fmt.Errorf("%w: managed workspace not found; run hufu first or pass --workspace: %v", inspectpkg.ErrInvalidQuery, err)
		}
		workspace = resolved
	}
	if workspace == "" {
		return inspectpkg.InspectQuery{}, fmt.Errorf("%w: managed workspace not found; run hufu first or pass --workspace", inspectpkg.ErrInvalidQuery)
	}
	return inspectpkg.InspectQuery{
		Workspace: workspace,
		RunID:     strings.TrimSpace(options.run),
		BranchID:  strings.TrimSpace(options.branch),
	}, nil
}

func finishExplain(command *cobra.Command, format string, query inspectpkg.InspectQuery, envelope *inspectpkg.Envelope, explainErr error) error {
	if explainErr != nil {
		command.Root().SilenceErrors = true
		command.Root().SilenceUsage = true
		exitErr := &inspectExitError{code: inspectpkg.ExitCodeFor(explainErr), err: fmt.Errorf("hufu explain: %w", explainErr)}
		if format == "json" {
			encoder := json.NewEncoder(command.OutOrStdout())
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(inspectpkg.OverviewFailure(query, explainErr)); err != nil {
				return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: encode JSON failure: %w", err)}
			}
			return exitErr
		}
		failure := inspectpkg.OverviewFailure(query, explainErr).Data.(inspectpkg.OverviewData)
		if err := renderOverviewError(command.ErrOrStderr(), failure.Error, query.Workspace); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: render error: %w", err)}
		}
		return exitErr
	}
	if envelope == nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: overview is unavailable")}
	}
	if format == "json" {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(envelope); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: encode JSON: %w", err)}
		}
		return nil
	}
	data, ok := envelope.Data.(inspectpkg.OverviewData)
	if !ok || data.Snapshot == nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: overview snapshot is unavailable")}
	}
	if err := renderExplainText(command.OutOrStdout(), data.Snapshot); err != nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu explain: render text: %w", err)}
	}
	return nil
}

func renderExplainText(writer io.Writer, snapshot *operatorpkg.OperatorSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("nil operator snapshot")
	}
	summary := operatorpkg.BuildSummaryForShell(*snapshot, filepath.Base(os.Getenv("SHELL")))
	progress := summarizeExplainTaskProgress(snapshot.Activity.RawTaskStates)
	if _, err := fmt.Fprintf(writer,
		"Hufu progress\nWorkspace: %s\nTeam: %s\nRun: %s\nBranch: %s\nState: %s\nTasks: %d done · %d active · %d waiting · %d need attention · %d skipped (%d total)\nWhat: %s\nNext: %s\nEvidence: %s\nIntegrity: %s\n",
		safeOverviewValue(snapshot.Scope.WorkspaceExact),
		safeOverviewValue(valueOrUnavailable(snapshot.Scope.TeamName)),
		safeOverviewValue(valueOrUnavailable(snapshot.Scope.RunID)),
		safeOverviewValue(valueOrUnavailable(snapshot.Scope.BranchID)),
		summary.State,
		progress.done, progress.active, progress.waiting, progress.attention, progress.skipped, progress.total,
		summary.What, summary.Next, summary.Data, safeOverviewValue(snapshot.Integrity.Status),
	); err != nil {
		return err
	}
	if len(snapshot.Blockers) > 0 {
		if _, err := fmt.Fprintln(writer, "Blockers:"); err != nil {
			return err
		}
		for _, blocker := range snapshot.Blockers {
			if _, err := fmt.Fprintf(writer, "  - [%s] %s (ref: %s)\n",
				safeOverviewValue(blocker.Code), safeOverviewValue(blocker.Message), safeOverviewValue(valueOrUnavailable(blocker.Ref))); err != nil {
				return err
			}
		}
	}
	if len(snapshot.LatestChanges) > 0 {
		if _, err := fmt.Fprintln(writer, "Recent changes:"); err != nil {
			return err
		}
		for _, change := range snapshot.LatestChanges {
			if _, err := fmt.Fprintf(writer, "  - #%d %s · status=%s · reason=%s · refs=%s\n",
				change.EventOrdinal, safeOverviewValue(change.Kind), safeOverviewValue(valueOrUnavailable(change.Status)),
				safeOverviewValue(valueOrUnavailable(change.ReasonCode)), safeOverviewValue(refsOrNone(change.Refs))); err != nil {
				return err
			}
		}
	}
	return nil
}

func summarizeExplainTaskProgress(states []string) explainTaskProgress {
	progress := explainTaskProgress{total: len(states)}
	for _, state := range states {
		switch team.TaskStatus(state) {
		case team.TaskDone:
			progress.done++
		case team.TaskInProgress, team.TaskVerifying:
			progress.active++
		case team.TaskPending, team.TaskPlanned, team.TaskPaused:
			progress.waiting++
		case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
			progress.attention++
		case team.TaskSkipped:
			progress.skipped++
		default:
			progress.attention++
		}
	}
	return progress
}
