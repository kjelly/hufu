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
)

type inspectCLIOptions struct {
	workspace string
	branch    string
	session   string
	format    string
	output    string
}

type inspectExitError struct {
	code int
	err  error
}

func (e *inspectExitError) Error() string        { return e.err.Error() }
func (e *inspectExitError) Unwrap() error        { return e.err }
func (e *inspectExitError) ProcessExitCode() int { return e.code }

func newInspectCommand() *cobra.Command {
	options := &inspectCLIOptions{format: string(inspectpkg.FormatText)}
	command := &cobra.Command{
		Use:   "inspect",
		Short: "Inspect persisted run facts without executing runtime behavior",
		Long: `Inspect is a read-only facade over Hufu's canonical event, evidence,
context, decision, and terminal projections. It never executes agents,
providers, verifiers, migrations, or recovery actions, and it does not replace
the detailed audit, context, or decision maintenance commands.`,
		Example: `  hufu inspect run run-123 --workspace ./workspace
  hufu inspect overview --workspace ./workspace --output json
  hufu inspect task task-7 --run run-123 --format json
  hufu inspect trace run-123 --branch incident-fix
  hufu inspect replay run-123 --format json
  hufu inspect storage --workspace ./workspace --format json`,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
	}
	command.PersistentFlags().StringVarP(&options.workspace, "workspace", "w", "", "Workspace directory (default: active managed workspace)")
	command.PersistentFlags().StringVar(&options.branch, "branch", "", "Exact branch ID, name, or branch label (default: active branch)")
	command.PersistentFlags().StringVar(&options.session, "session", "", "Optional exact session ID filter")
	command.PersistentFlags().StringVar(&options.format, "format", string(inspectpkg.FormatText), "Output format: text or json")
	command.PersistentFlags().StringVar(&options.output, "output", "", "Output format alias: text or json")
	registerStaticFlagCompletion(command, "format", []string{string(inspectpkg.FormatText), string(inspectpkg.FormatJSON)})
	registerStaticFlagCompletion(command, "output", []string{string(inspectpkg.FormatText), string(inspectpkg.FormatJSON)})
	command.AddCommand(
		newInspectOverviewCommand(options),
		newInspectRunCommand(options),
		newInspectTaskCommand(options),
		newInspectEvidenceCommand(options),
		newInspectContextCommand(options),
		newInspectTraceCommand(options),
		newInspectReplayCommand(options),
		newInspectStorageCommand(options),
	)
	return command
}

func newInspectOverviewCommand(options *inspectCLIOptions) *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:               "overview",
		Short:             "Show a read-only operator overview for one exact workspace",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = runID
			envelope, inspectErr := inspectpkg.InspectOverview(command.Context(), query)
			return finishInspectOverview(command, format, query, envelope, inspectErr)
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Optional exact run ID (default: active binding or sole candidate)")
	return command
}

func newInspectStorageCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:               "storage",
		Short:             "Show read-only SQLite storage diagnostics",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			envelope, inspectErr := inspectpkg.InspectStorage(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func newInspectReplayCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:               "replay <run-id>",
		Short:             "Compare canonical in-memory replay with stored projections",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = args[0]
			envelope, inspectErr := inspectpkg.InspectReplay(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func newInspectTraceCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:               "trace <run-id>",
		Short:             "Show a unified event-anchored timeline of persisted run facts",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = args[0]
			envelope, inspectErr := inspectpkg.InspectTrace(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func newInspectEvidenceCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:               "evidence <run-id>",
		Short:             "Show audit-verified evidence metadata without artifact content",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = args[0]
			envelope, inspectErr := inspectpkg.InspectEvidence(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func newInspectContextCommand(options *inspectCLIOptions) *cobra.Command {
	var runID, projectID, teamID, agentID string
	var attempt int
	var showContent, allAgents bool
	command := &cobra.Command{
		Use:               "context <task-id>",
		Short:             "Show context and memory injection metadata for one task",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = runID
			query.TaskID = args[0]
			query.Attempt = attempt
			query.ProjectID = projectID
			query.TeamID = teamID
			query.AgentID = agentID
			envelope, inspectErr := inspectpkg.InspectContext(command.Context(), query, inspectpkg.ContextOptions{ShowContent: showContent, AllAgents: allAgents})
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run ID containing the task (required)")
	command.Flags().IntVar(&attempt, "attempt", 0, "Optional positive attempt number")
	command.Flags().StringVar(&projectID, "project", "", "Canonical context project ID (required)")
	command.Flags().StringVar(&teamID, "team", "", "Optional exact context team scope")
	command.Flags().StringVar(&agentID, "agent", "", "Worker identity authorized to inspect private context")
	command.Flags().BoolVar(&allAgents, "all-agents", false, "Explicit maintenance view of private context for all workers")
	command.Flags().BoolVar(&showContent, "show-content", false, "Include authorized, safely redacted context content")
	return command
}

func newInspectRunCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:               "run <run-id>",
		Short:             "Show a canonical run outcome and task summary",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = args[0]
			envelope, inspectErr := inspectpkg.InspectRun(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
}

func newInspectTaskCommand(options *inspectCLIOptions) *cobra.Command {
	var runID string
	var attempt int
	command := &cobra.Command{
		Use:               "task <task-id>",
		Short:             "Show a canonical task and its persisted attempts",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := options.resolveFormat(command)
			if err != nil {
				return err
			}
			query, err := options.query()
			if err != nil {
				return err
			}
			query.RunID = runID
			query.TaskID = args[0]
			query.Attempt = attempt
			envelope, inspectErr := inspectpkg.InspectTask(command.Context(), query)
			return finishInspect(command, format, envelope, inspectErr)
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run ID containing the task (required)")
	command.Flags().IntVar(&attempt, "attempt", 0, "Optional positive attempt number")
	return command
}

func (options *inspectCLIOptions) resolveFormat(command *cobra.Command) (string, error) {
	format, err := resolveOutputAlias("format", options.format, flagChanged(command, "format"), "output", options.output, flagChanged(command, "output"), string(inspectpkg.FormatText), []string{string(inspectpkg.FormatText), string(inspectpkg.FormatJSON)})
	if err != nil {
		if strings.HasPrefix(err.Error(), "invalid --format") {
			value := options.format
			if flagChanged(command, "output") {
				value = options.output
			}
			err = fmt.Errorf("invalid format %q (must be text or json)", value)
		}
		return "", &inspectExitError{code: inspectpkg.ExitUsage, err: fmt.Errorf("hufu inspect: %w", err)}
	}
	return format, nil
}

func flagChanged(command *cobra.Command, name string) bool {
	flag := command.Flag(name)
	return flag != nil && flag.Changed
}

func (options *inspectCLIOptions) query() (inspectpkg.InspectQuery, error) {
	workspace := options.workspace
	if strings.TrimSpace(workspace) == "" {
		workspace = getWorkspace()
	}
	workspace, err := requireResolvedWorkspace(workspace)
	if err != nil {
		return inspectpkg.InspectQuery{}, &inspectExitError{code: inspectpkg.ExitUsage, err: fmt.Errorf("hufu inspect: %w", err)}
	}
	return inspectpkg.InspectQuery{
		Workspace: workspace,
		BranchID:  options.branch,
		SessionID: options.session,
	}, nil
}

func finishInspect(command *cobra.Command, format string, envelope *inspectpkg.Envelope, err error) error {
	if err != nil {
		return &inspectExitError{code: inspectpkg.ExitCodeFor(err), err: fmt.Errorf("hufu inspect: %w", err)}
	}
	switch inspectpkg.Format(strings.ToLower(strings.TrimSpace(format))) {
	case inspectpkg.FormatJSON:
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(envelope); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect: encode JSON: %w", err)}
		}
		return inspectProjectionExit(command, envelope)
	case inspectpkg.FormatText:
		if err := renderInspectText(command.OutOrStdout(), envelope); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect: render text: %w", err)}
		}
		return inspectProjectionExit(command, envelope)
	default:
		return &inspectExitError{code: inspectpkg.ExitUsage, err: fmt.Errorf("hufu inspect: invalid format %q (must be text or json)", format)}
	}
}

func finishInspectOverview(command *cobra.Command, format string, query inspectpkg.InspectQuery, envelope *inspectpkg.Envelope, inspectErr error) error {
	if inspectErr == nil {
		return finishInspect(command, format, envelope, nil)
	}
	command.Root().SilenceErrors = true
	command.Root().SilenceUsage = true
	exitErr := &inspectExitError{code: inspectpkg.ExitCodeFor(inspectErr), err: fmt.Errorf("hufu inspect overview: %w", inspectErr)}
	if inspectpkg.Format(format) == inspectpkg.FormatJSON {
		failure := inspectpkg.OverviewFailure(query, inspectErr)
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(failure); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect overview: encode JSON failure: %w", err)}
		}
		return exitErr
	}
	data := inspectpkg.OverviewFailure(query, inspectErr).Data.(inspectpkg.OverviewData)
	if err := renderOverviewError(command.ErrOrStderr(), data.Error, query.Workspace); err != nil {
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect overview: render error: %w", err)}
	}
	return exitErr
}

func renderOverviewError(writer io.Writer, view *inspectpkg.OperatorErrorView, target string) error {
	if view == nil {
		return fmt.Errorf("nil operator error")
	}
	_, err := fmt.Fprintf(writer, "Error [%s]\nState: Operation did not start; no data was modified.\nWhat: %s.\nTarget: %s\nNext: Verify the exact workspace and selectors with inspect overview.\n",
		view.Code, view.Message, valueOrUnavailable(target))
	return err
}

func inspectProjectionExit(command *cobra.Command, envelope *inspectpkg.Envelope) error {
	if envelope == nil || envelope.Kind != inspectpkg.KindReplay {
		return nil
	}
	data, ok := envelope.Data.(inspectpkg.ReplayData)
	if ok && data.OverallStatus == "drift" {
		command.Root().SilenceUsage = true
		return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect replay: stored projection drift detected")}
	}
	return nil
}

func renderInspectText(writer io.Writer, envelope *inspectpkg.Envelope) error {
	if envelope == nil {
		return fmt.Errorf("nil inspect envelope")
	}
	switch data := envelope.Data.(type) {
	case inspectpkg.OverviewData:
		if data.Snapshot == nil {
			return fmt.Errorf("overview snapshot is unavailable")
		}
		snapshot := data.Snapshot
		summary := operatorpkg.BuildSummaryForShell(*snapshot, filepath.Base(os.Getenv("SHELL")))
		if _, err := fmt.Fprintf(writer, "Workspace: %s\nTeam: %s\nRun: %s\nInvocation: %s\nBranch: %s\nState: %s\nWhat: %s\nNext: %s\nData: %s\nActivity: %s\nAttention: %s\nOutcome: %s\nAcceptance: %s\nCompletion: %s\nIntegrity: %s\n",
			safeOverviewValue(snapshot.Scope.WorkspaceExact), safeOverviewValue(valueOrUnavailable(snapshot.Scope.TeamName)), safeOverviewValue(snapshot.Scope.RunID),
			safeOverviewValue(valueOrUnavailable(snapshot.Scope.InvocationID)), safeOverviewValue(snapshot.Scope.BranchID), summary.State, summary.What, summary.Next, summary.Data,
			snapshot.Activity.State, snapshot.Attention,
			valueOrUnavailable(snapshot.Outcome.RunOutcome), snapshot.Outcome.AcceptanceState,
			snapshot.Outcome.CompletionState, snapshot.Integrity.Status); err != nil {
			return err
		}
		for _, change := range snapshot.LatestChanges {
			if _, err := fmt.Fprintf(writer, "Change %d: %s status=%s reason=%s refs=%s\n",
				change.EventOrdinal, safeOverviewValue(change.Kind), safeOverviewValue(valueOrUnavailable(change.Status)),
				safeOverviewValue(valueOrUnavailable(change.ReasonCode)), safeOverviewValue(refsOrNone(change.Refs))); err != nil {
				return err
			}
		}
		return nil
	case inspectpkg.RunData:
		_, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\nOutcome: %s\nAcceptance: %s\nCompletion: %s\nTasks: %d total, %d done, %d unresolved\nAttempts: %d total, %d failed\nEvidence refs: %s\n",
			data.RunID, envelope.Query.BranchID, valueOrUnavailable(data.Outcome), data.Acceptance, data.Completion,
			data.TaskSummary.Total, data.TaskSummary.Done, data.TaskSummary.Unresolved,
			data.AttemptSummary.Total, data.AttemptSummary.Failed, refsOrNone(data.EvidenceRefs))
		return err
	case inspectpkg.TaskData:
		if _, err := fmt.Fprintf(writer, "Run: %s\nTask: %s\nBranch: %s\nStatus: %s\nPhase: %s\nAgent: %s\nExecution target: %s\n",
			data.RunID, data.TaskID, envelope.Query.BranchID, data.Status, valueOrUnavailable(data.Phase),
			valueOrUnavailable(data.AgentID), valueOrUnavailable(data.ExecutionTarget)); err != nil {
			return err
		}
		for _, attempt := range data.Attempts {
			if _, err := fmt.Fprintf(writer, "Attempt %d: model_execution_id=%s producer=%s execution_target=%s backend=%s exit_code=%s verification=%s winning=%t\n",
				attempt.Attempt, valueOrUnavailable(attempt.ModelExecutionID), valueOrUnavailable(attempt.ProducerID),
				valueOrUnavailable(attempt.ExecutionTarget), valueOrUnavailable(attempt.Backend), optionalInt(attempt.ExitCode), attempt.VerificationStatus, attempt.Winning); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(writer, "Artifact refs: %s\nContext refs: %s\nMemory refs: %s\n",
			refsOrNone(data.ArtifactRefs), refsOrNone(data.ContextRefs), refsOrNone(data.MemoryRefs))
		if err == nil && data.KnowledgeCoverage != nil {
			outcome := data.KnowledgeCoverage.OutcomeCoverage
			invariants := data.KnowledgeCoverage.InvariantCoverage
			coveredPaths := max(0, invariants.TouchedPathCount-invariants.UncoveredPathCount)
			_, err = fmt.Fprintf(writer, "Knowledge: %d known, %d assumed, %d stale; invariants: %d/%d paths covered\n", outcome.KnownCount, outcome.AssumedCount, outcome.StaleCount, coveredPaths, invariants.TouchedPathCount)
		}
		return err
	case inspectpkg.EvidenceData:
		if _, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\nManifest: %s (%s)\nAudit verdict: %s\nAcceptance: %s\n",
			data.RunID, envelope.Query.BranchID, valueOrUnavailable(data.Manifest.Hash), data.Manifest.Status,
			data.Verification.Verdict, data.Acceptance); err != nil {
			return err
		}
		for _, requirement := range data.Requirements {
			if _, err := fmt.Fprintf(writer, "Requirement %s: %s validator=%s artifacts=%d\n",
				requirement.RequirementID, requirement.Status, valueOrUnavailable(requirement.Validator), len(requirement.ArtifactRefs)); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(writer, "Artifacts: %d\nFindings: %d\n", len(data.ArtifactRefs), len(data.Findings))
		return err
	case inspectpkg.ContextData:
		if _, err := fmt.Fprintf(writer, "Run: %s\nTask: %s\nBranch: %s\nAuthorization: %s\nAttempts: %s\nContext manifests: %d\nMemory manifests: %d\n",
			data.RunID, data.TaskID, envelope.Query.BranchID, data.Authorization.Status,
			intsOrNone(data.Attempts), len(data.Manifests), len(data.MemoryManifests)); err != nil {
			return err
		}
		for _, item := range data.Items {
			if _, err := fmt.Fprintf(writer, "Context %s: available=%t kind=%s lifecycle=%s agent=%s\n",
				item.ID, item.Available, item.Kind, item.Lifecycle, valueOrUnavailable(item.Scope.AgentID)); err != nil {
				return err
			}
			if item.Content != "" {
				if _, err := fmt.Fprintf(writer, "  %s\n", item.Content); err != nil {
					return err
				}
			}
		}
		return nil
	case inspectpkg.TraceData:
		if _, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\n", data.RunID, envelope.Query.BranchID); err != nil {
			return err
		}
		for _, entry := range data.Entries {
			ordinal := entry.Ref.EventOrdinal
			if ordinal == 0 {
				ordinal = entry.AnchorEventOrdinal
			}
			if _, err := fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\n", ordinal, entry.Ref.Source, entry.Kind, valueOrUnavailable(entry.Status), valueOrUnavailable(entry.ReasonCode)); err != nil {
				return err
			}
		}
		return nil
	case inspectpkg.StorageData:
		_, err := fmt.Fprintf(writer, "Page count: %d\nFreelist count: %d\nPage size: %d\nJournal mode: %s\nWAL autocheckpoint: %d\nSchema version: %d\nDatabase bytes: %d\nWAL bytes: %d\nFTS rows: %d\nContext rows: %d\n",
			data.PageCount, data.FreelistCount, data.PageSize, data.JournalMode, data.WALAutoCheckpoint,
			data.SchemaVersion, data.DatabaseBytes, data.WALBytes, data.FTSRows, data.ContextRows)
		return err
	case inspectpkg.ReplayData:
		if _, err := fmt.Fprintf(writer, "Run: %s\nBranch: %s\nEvent chain: %s\nOverall: %s\n", data.RunID, envelope.Query.BranchID, data.EventChain, data.OverallStatus); err != nil {
			return err
		}
		for _, check := range data.Checks {
			if _, err := fmt.Fprintf(writer, "%s: %s reason=%s diffs=%s\n", check.Name, check.Status, valueOrUnavailable(check.ReasonCode), refsOrNone(check.DiffPaths)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported inspect data %T", envelope.Data)
	}
}

func safeOverviewValue(value string) string {
	return operatorpkg.SafeDisplayText(value, 320)
}

func intsOrNone(values []int) string {
	if len(values) == 0 {
		return "none"
	}
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = fmt.Sprintf("%d", value)
	}
	return strings.Join(parts, ", ")
}

func valueOrUnavailable(value string) string {
	if value == "" {
		return "unavailable"
	}
	return value
}

func refsOrNone(refs []string) string {
	if len(refs) == 0 {
		return "none"
	}
	return strings.Join(refs, ", ")
}

func optionalInt(value *int) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%d", *value)
}
