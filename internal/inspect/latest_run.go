package inspect

import (
	"context"
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// LatestRunSelection names the stable selection contract: the last run_started
// across all branches, rather than the last updated task or active branch.
const LatestRunSelection = "latest_started_all_branches"

// LatestRun describes the last run_started in a workspace's validated global
// event chain, including inactive branches. It is not an OS liveness check.
type LatestRun struct {
	Workspace    string         `json:"workspace"`
	Team         string         `json:"team"`
	InvocationID string         `json:"invocation_id"`
	SessionID    string         `json:"session_id"`
	BranchID     string         `json:"branch_id"`
	StartedAt    time.Time      `json:"started_at"`
	ObservedAt   time.Time      `json:"observed_at"`
	EventID      string         `json:"event_id"`
	TaskStates   map[string]int `json:"task_states"`
	Run          RunData        `json:"run"`
}

// InspectLatestRun returns nil when no run has started. All data is read-only;
// task counts and outcomes come from the same verified branch event snapshot.
func InspectLatestRun(ctx context.Context, workspace string) (*LatestRun, error) {
	query := InspectQuery{Workspace: workspace}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	var latest *team.RunEvent
	for _, indexed := range lineage.GlobalEvents {
		if indexed.Event.Type == string(team.EventRunStarted) {
			latest = &indexed.Event
		}
	}
	if latest == nil {
		return nil, nil
	}
	started, err := time.Parse(time.RFC3339Nano, latest.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: run start timestamp is invalid", ErrIntegrity)
	}
	query.RunID, query.SessionID, query.BranchID = latest.RunID, latest.SessionID, latest.BranchID
	// Resolve the branch explicitly, even when the user has checked out an older
	// branch. Never change checkout or use a mutable session checkpoint.
	if query.BranchID != lineage.BranchID {
		lineage, err = LoadLineage(ctx, query)
		if err != nil {
			return nil, err
		}
	}
	data, _, tasks, err := projectRunData(ctx, query, lineage)
	if err != nil {
		return nil, err
	}
	invocation, err := persistedInvocationID(lineage, query.RunID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]int)
	for _, task := range tasks {
		if task != nil {
			states[string(task.Status)]++
		}
	}
	return &LatestRun{
		Workspace: workspace, Team: lineage.SelectedTeam, InvocationID: invocation,
		SessionID: latest.SessionID, BranchID: lineage.BranchID, StartedAt: started,
		ObservedAt: time.Now().UTC(), EventID: latest.ID, TaskStates: states, Run: data,
	}, nil
}
