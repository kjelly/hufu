package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

var (
	statusWorkspace string
	statusTeam      string
	statusJSON      bool
	statusWorkers   bool
	statusVerbose   bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current workspace session status",
	Args:  cobra.NoArgs,
	RunE:  runStatus,
}

type workspaceStatus struct {
	Failures           []team.FailureEventPayload `json:"failures,omitempty"`
	Workspace          string                     `json:"workspace"`
	Team               string                     `json:"team,omitempty"`
	RunID              string                     `json:"run_id,omitempty"`
	InvocationID       string                     `json:"invocation_id,omitempty"`
	BranchID           string                     `json:"branch_id,omitempty"`
	Session            string                     `json:"session"`
	CreatedAt          string                     `json:"created_at,omitempty"`
	UpdatedAt          string                     `json:"updated_at,omitempty"`
	Rounds             int                        `json:"rounds"`
	Total              int                        `json:"total"`
	Done               int                        `json:"done"`
	Error              int                        `json:"error"`
	Skipped            int                        `json:"skipped"`
	Pending            int                        `json:"pending"`
	InProgress         int                        `json:"in_progress"`
	Verifying          int                        `json:"verifying"`
	Blocked            int                        `json:"blocked"`
	Planned            int                        `json:"planned"`
	Paused             int                        `json:"paused"`
	ProtocolIncomplete int                        `json:"protocol_incomplete"`
	Unknown            int                        `json:"unknown"`
	// Workers is present only with --workers.
	Workers     *inspectpkg.WorkerAttempts   `json:"workers,omitempty"`
	Recovery    *workspaceRecovery           `json:"recovery,omitempty"`
	DerivedFork *operatorpkg.DerivedForkView `json:"derived_fork,omitempty"`
}

func init() {
	statusCmd.Flags().StringVarP(&statusWorkspace, "workspace", "w", "", "Workspace directory (default: active managed workspace)")
	statusCmd.Flags().StringVar(&statusTeam, "team", "", "Team name when the project has multiple active managed workspaces")
	addTextJSONOutputFlag(statusCmd)
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Alias for --output json")
	statusCmd.Flags().BoolVar(&statusWorkers, "workers", false, "Show every worker attempt of the active branch")
	statusCmd.Flags().BoolVar(&statusVerbose, "verbose", false, "With --workers, show each attempt's route, usage, result contract, failure, world, and identity")
}

func runStatus(command *cobra.Command, _ []string) error {
	output, err := resolveTextJSONOutput(command, statusJSON)
	if err != nil {
		return err
	}
	ws := statusWorkspace
	if ws == "" {
		teamName := statusTeam
		if teamName == "" {
			teamName = opts.agentTeamName
		}
		ws, err = resolveExistingManagedWorkspacePath(command.Context(), runtimeStartDir(), teamName)
		if err != nil {
			if errors.Is(err, workspacepkg.ErrNotFound) {
				return fmt.Errorf("managed workspace not found: %w\nNext: Use 'hufu workspace status' to check registered team workspaces", err)
			}
			return fmt.Errorf("resolve managed workspace: %w\nNext: Use 'hufu workspace status' to check registered team workspaces", err)
		}
	}
	info, err := os.Stat(ws)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace directory %q does not exist\nNext: Check the path or use 'hufu workspace status' to find a registered workspace", ws)
	}
	if err != nil {
		return fmt.Errorf("cannot access workspace %q: %w\nNext: Check directory permissions", ws, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory\nNext: Use the directory that contains runtime.sqlite", ws)
	}
	data, exists, err := team.LoadSessionReadOnly(ws)
	if err != nil {
		return fmt.Errorf("cannot read the session from %q: %w\nNext: Check permissions on runtime.sqlite and its WAL. Run SQLite PRAGMA quick_check on a copy of those files to check database integrity", runtimestore.Path(ws), err)
	}
	if !exists {
		return fmt.Errorf("no saved session in workspace %q\nNext: Check for runtime.sqlite in that directory or use 'hufu workspace status' to locate recorded runs", ws)
	}
	status := summarizeWorkspaceSession(ws, data)
	if err := attachCanonicalWorkspaceStatus(command.Context(), ws, &status); err != nil {
		return err
	}
	if statusWorkers {
		var metrics *team.RunMetrics
		if data.RunResult != nil {
			metrics = &data.RunResult.Metrics
		}
		hub, err := inspectpkg.LoadWorkerAttempts(command.Context(), ws, metrics, time.Now())
		if err != nil && !errors.Is(err, inspectpkg.ErrNotFound) {
			return fmt.Errorf("load worker attempts: %w", err)
		}
		if hub.Attempts == nil {
			hub.Attempts = []inspectpkg.WorkerAttemptView{}
		}
		status.Workers = &hub
	}
	if output == "json" {
		return json.NewEncoder(command.OutOrStdout()).Encode(status)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Workspace:  %s\n", status.Workspace)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Team:       %s\n", safeOverviewValue(valueOrUnavailable(status.Team)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Run:        %s\n", safeOverviewValue(valueOrUnavailable(status.RunID)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Invocation: %s\n", safeOverviewValue(valueOrUnavailable(status.InvocationID)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Branch:     %s\n", safeOverviewValue(valueOrUnavailable(status.BranchID)))
	writeDerivedForkStatus(command.OutOrStdout(), status.DerivedFork)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Session:    %s\n", status.Session)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Updated:    %s\n", status.UpdatedAt)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Rounds:     %d\n", status.Rounds)
	writer := command.OutOrStdout()
	_, _ = fmt.Fprintf(writer, "Tasks:      %d done · %d error · %d blocked · %d skipped · %d pending · %d in progress · %d verifying",
		status.Done, status.Error, status.Blocked, status.Skipped, status.Pending, status.InProgress, status.Verifying)
	for _, state := range []struct {
		count int
		label string
	}{
		{status.Planned, "planned"}, {status.Paused, "paused"},
		{status.ProtocolIncomplete, "protocol incomplete"}, {status.Unknown, "unknown"},
	} {
		if state.count > 0 {
			_, _ = fmt.Fprintf(writer, " · %d %s", state.count, state.label)
		}
	}
	_, _ = fmt.Fprintf(writer, " (%d total)\n", status.Total)
	for _, failure := range status.Failures {
		_, _ = fmt.Fprintln(command.OutOrStdout(), team.RenderFailureText(&failure))
	}
	writeWorkspaceRecovery(command.OutOrStdout(), status.Recovery)
	if status.Workers != nil {
		writeWorkerAttempts(command.OutOrStdout(), *status.Workers, statusVerbose)
	}
	return nil
}

func attachCanonicalWorkspaceStatus(ctx context.Context, workspace string, status *workspaceStatus) error {
	envelope, err := inspectpkg.InspectOverview(ctx, inspectpkg.InspectQuery{Workspace: workspace})
	if err != nil {
		if errors.Is(err, inspectpkg.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("read canonical status from %q: %w\nNext: Check storage with 'hufu inspect storage' for this workspace", runtimestore.Path(workspace), err)
	}
	data, ok := envelope.Data.(inspectpkg.OverviewData)
	if !ok || data.Snapshot == nil {
		return fmt.Errorf("resolve canonical status identity: overview snapshot is unavailable")
	}
	applyWorkspaceStatusScope(status, data.Snapshot.Scope)
	status.Recovery = workspaceRecoveryFromSnapshot(*data.Snapshot)
	status.DerivedFork = data.Snapshot.DerivedFork
	return nil
}

func applyWorkspaceStatusScope(status *workspaceStatus, scope operatorpkg.ResolvedScope) {
	if status == nil {
		return
	}
	status.Team = scope.TeamName
	status.RunID = scope.RunID
	status.InvocationID = scope.InvocationID
	status.BranchID = scope.BranchID
}

func summarizeWorkspaceSession(workspace string, session *team.SessionData) workspaceStatus {
	status := workspaceStatus{
		Workspace: workspace,
		Failures:  team.FailureEventsFromTodos(session.Tasks),
		Session:   runtimestore.Path(workspace),
		CreatedAt: session.CreatedAt,
		UpdatedAt: session.UpdatedAt,
		Rounds:    session.Rounds,
	}
	for _, item := range session.Tasks {
		if item == nil {
			continue
		}
		status.Total++
		switch item.Status {
		case team.TaskDone:
			status.Done++
		case team.TaskError:
			status.Error++
		case team.TaskBlocked:
			status.Blocked++
		case team.TaskSkipped:
			status.Skipped++
		case team.TaskPending:
			status.Pending++
		case team.TaskInProgress:
			status.InProgress++
		case team.TaskVerifying:
			status.Verifying++
		case team.TaskPlanned:
			status.Planned++
		case team.TaskPaused:
			status.Paused++
		case team.TaskProtocolIncomplete:
			status.ProtocolIncomplete++
		default:
			status.Unknown++
		}
	}
	return status
}
