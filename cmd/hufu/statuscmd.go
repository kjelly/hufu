package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/team"
)

var (
	statusWorkspace string
	statusJSON      bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current workspace session status",
	Args:  cobra.NoArgs,
	RunE:  runStatus,
}

type workspaceStatus struct {
	Workspace string `json:"workspace"`
	Session   string `json:"session"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Rounds    int    `json:"rounds"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Error     int    `json:"error"`
	Skipped   int    `json:"skipped"`
	Pending   int    `json:"pending"`
}

func init() {
	statusCmd.Flags().StringVarP(&statusWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Write machine-readable JSON to stdout")
}

func runStatus(_ *cobra.Command, _ []string) error {
	ws := statusWorkspace
	if ws == "" {
		ws = getWorkspace()
	}
	data := team.LoadSession(ws)
	if data == nil {
		return fmt.Errorf("no session found in %s; run hufu first or pass --workspace", ws)
	}
	status := summarizeWorkspaceSession(ws, data)
	if statusJSON {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Workspace: %s\n", status.Workspace)
	_, _ = fmt.Fprintf(os.Stdout, "Session:   %s\n", status.Session)
	_, _ = fmt.Fprintf(os.Stdout, "Updated:   %s\n", status.UpdatedAt)
	_, _ = fmt.Fprintf(os.Stdout, "Rounds:    %d\n", status.Rounds)
	_, _ = fmt.Fprintf(os.Stdout, "Tasks:     %d done · %d error · %d skipped · %d pending (%d total)\n", status.Done, status.Error, status.Skipped, status.Pending, status.Total)
	return nil
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
