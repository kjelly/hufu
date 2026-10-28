package main

import (
	"context"
	"fmt"
	"strings"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

type canonicalHistoricalTaskReader struct{}

func (canonicalHistoricalTaskReader) Load(ctx context.Context, scope team.HistoricalTaskScope) ([]team.HistoricalTaskResult, error) {
	if strings.TrimSpace(scope.Workspace) == "" || strings.TrimSpace(scope.Team) == "" || strings.TrimSpace(scope.Branch) == "" {
		return nil, fmt.Errorf("canonical task history requires exact workspace, team and branch")
	}
	snapshot, err := inspectpkg.LoadHistoricalResults(ctx, inspectpkg.HistoricalResultScope{Workspace: scope.Workspace, Team: scope.Team, Branch: scope.Branch})
	if err != nil {
		return nil, err
	}
	rows := make([]team.HistoricalTaskResult, 0, len(snapshot.Results))
	for _, row := range snapshot.Results {
		if row.Kind != "task" {
			continue
		}
		rows = append(rows, team.HistoricalTaskResult{
			ResultID: row.ResultID, Team: row.Team, SessionID: row.SessionID, SourceBranchID: row.SourceBranchID,
			RunID: row.RunID, OriginRunID: row.OriginRunID, TaskID: row.TaskID, Agent: row.Agent, Attempt: row.Attempt,
			Status: row.Status, CurrentStatus: row.CurrentStatus, Validity: row.Validity, Description: row.Description,
			Summary: row.Summary, Output: row.Output, TypedResult: row.TypedResult,
			SourceEventID: row.SourceEventID, SourceHash: row.SourceHash,
		})
	}
	return rows, nil
}
