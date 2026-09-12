package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

type inspectCLIOptions struct {
	workspace string
	branch    string
	session   string
	format    string
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
		Args:  cobra.NoArgs,
	}
	command.PersistentFlags().StringVarP(&options.workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	command.PersistentFlags().StringVar(&options.branch, "branch", "", "Exact branch ID, name, or branch label (default: active branch)")
	command.PersistentFlags().StringVar(&options.session, "session", "", "Optional exact session ID filter")
	command.PersistentFlags().StringVar(&options.format, "format", string(inspectpkg.FormatText), "Output format: text or json")
	command.AddCommand(newInspectRunCommand(options), newInspectTaskCommand(options))
	return command
}

func newInspectRunCommand(options *inspectCLIOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "run <run-id>",
		Short: "Show a canonical run outcome and task summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			query := options.query()
			query.RunID = args[0]
			envelope, err := inspectpkg.InspectRun(context.Background(), query)
			return finishInspect(command, options.format, envelope, err)
		},
	}
}

func newInspectTaskCommand(options *inspectCLIOptions) *cobra.Command {
	var runID string
	var attempt int
	command := &cobra.Command{
		Use:   "task <task-id>",
		Short: "Show a canonical task and its persisted attempts",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			query := options.query()
			query.RunID = runID
			query.TaskID = args[0]
			query.Attempt = attempt
			envelope, err := inspectpkg.InspectTask(context.Background(), query)
			return finishInspect(command, options.format, envelope, err)
		},
	}
	command.Flags().StringVar(&runID, "run", "", "Run ID containing the task (required)")
	command.Flags().IntVar(&attempt, "attempt", 0, "Optional positive attempt number")
	return command
}

func (options *inspectCLIOptions) query() inspectpkg.InspectQuery {
	workspace := options.workspace
	if strings.TrimSpace(workspace) == "" {
		workspace = getWorkspace()
	}
	return inspectpkg.InspectQuery{
		Workspace: workspace,
		BranchID:  options.branch,
		SessionID: options.session,
	}
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
		return nil
	case inspectpkg.FormatText:
		if err := renderInspectText(command.OutOrStdout(), envelope); err != nil {
			return &inspectExitError{code: inspectpkg.ExitIntegrity, err: fmt.Errorf("hufu inspect: render text: %w", err)}
		}
		return nil
	default:
		return &inspectExitError{code: inspectpkg.ExitUsage, err: fmt.Errorf("hufu inspect: invalid format %q (must be text or json)", format)}
	}
}

func renderInspectText(writer io.Writer, envelope *inspectpkg.Envelope) error {
	if envelope == nil {
		return fmt.Errorf("nil inspect envelope")
	}
	switch data := envelope.Data.(type) {
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
			if _, err := fmt.Fprintf(writer, "Attempt %d: model_execution_id=%s producer=%s backend=%s exit_code=%s verification=%s winning=%t\n",
				attempt.Attempt, valueOrUnavailable(attempt.ModelExecutionID), valueOrUnavailable(attempt.ProducerID),
				valueOrUnavailable(attempt.Backend), optionalInt(attempt.ExitCode), attempt.VerificationStatus, attempt.Winning); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(writer, "Artifact refs: %s\nContext refs: %s\nMemory refs: %s\n",
			refsOrNone(data.ArtifactRefs), refsOrNone(data.ContextRefs), refsOrNone(data.MemoryRefs))
		return err
	default:
		return fmt.Errorf("unsupported inspect data %T", envelope.Data)
	}
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
