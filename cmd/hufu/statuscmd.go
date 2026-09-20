package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

var (
	statusWorkspace string
	statusTeam      string
	statusJSON      bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current workspace session status",
	Args:  cobra.NoArgs,
	RunE:  runStatus,
}

type workspaceStatus struct {
	Workspace    string `json:"workspace"`
	Team         string `json:"team,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
	BranchID     string `json:"branch_id,omitempty"`
	Session      string `json:"session"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	Rounds       int    `json:"rounds"`
	Total        int    `json:"total"`
	Done         int    `json:"done"`
	Error        int    `json:"error"`
	Skipped      int    `json:"skipped"`
	Pending      int    `json:"pending"`
}

func init() {
	statusCmd.Flags().StringVarP(&statusWorkspace, "workspace", "w", "", "Workspace directory (default: active managed workspace)")
	statusCmd.Flags().StringVar(&statusTeam, "team", "", "Team name when the project has multiple active managed workspaces")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Write machine-readable JSON to stdout")
}

func runStatus(command *cobra.Command, _ []string) error {
	ws := statusWorkspace
	if ws == "" {
		teamName := statusTeam
		if teamName == "" {
			teamName = opts.agentTeamName
		}
		var err error
		ws, err = resolveExistingManagedWorkspacePath(command.Context(), runtimeStartDir(), teamName)
		if err != nil {
			if errors.Is(err, workspacepkg.ErrAmbiguous) {
				return err
			}
			return fmt.Errorf("managed workspace not found; run hufu first or pass --workspace: %w", err)
		}
	}
	if ws == "" {
		return fmt.Errorf("managed workspace not found; run hufu first or pass --workspace")
	}
	data := team.LoadSession(ws)
	if data == nil {
		return fmt.Errorf("no session found in %s; run hufu first or pass --workspace", ws)
	}
	status := summarizeWorkspaceSession(ws, data)
	if err := attachCanonicalWorkspaceStatus(command.Context(), ws, &status); err != nil {
		return err
	}
	if statusJSON {
		return json.NewEncoder(command.OutOrStdout()).Encode(status)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Workspace:  %s\n", status.Workspace)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Team:       %s\n", safeOverviewValue(valueOrUnavailable(status.Team)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Run:        %s\n", safeOverviewValue(valueOrUnavailable(status.RunID)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Invocation: %s\n", safeOverviewValue(valueOrUnavailable(status.InvocationID)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Branch:     %s\n", safeOverviewValue(valueOrUnavailable(status.BranchID)))
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Session:    %s\n", status.Session)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Updated:    %s\n", status.UpdatedAt)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Rounds:     %d\n", status.Rounds)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Tasks:      %d done · %d error · %d skipped · %d pending (%d total)\n", status.Done, status.Error, status.Skipped, status.Pending, status.Total)
	return nil
}

func attachCanonicalWorkspaceStatus(ctx context.Context, workspace string, status *workspaceStatus) error {
	envelope, err := inspectpkg.InspectOverview(ctx, inspectpkg.InspectQuery{Workspace: workspace})
	if err != nil {
		if errors.Is(err, inspectpkg.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("resolve canonical status identity: %w", err)
	}
	data, ok := envelope.Data.(inspectpkg.OverviewData)
	if !ok || data.Snapshot == nil {
		return fmt.Errorf("resolve canonical status identity: overview snapshot is unavailable")
	}
	applyWorkspaceStatusScope(status, data.Snapshot.Scope)
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
		Session:   filepath.Join(workspace, "session.json"),
		CreatedAt: session.CreatedAt,
		UpdatedAt: session.UpdatedAt,
		Rounds:    session.Rounds,
	}
	for _, item := range session.Tasks {
		status.Total++
		switch item.Status {
		case team.TaskDone:
			status.Done++
		case team.TaskError, team.TaskBlocked:
			status.Error++
		case team.TaskSkipped:
			status.Skipped++
		default:
			status.Pending++
		}
	}
	return status
}
