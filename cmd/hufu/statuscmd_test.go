package main

import (
	"testing"

	"github.com/anomalyco/hufu/internal/team"
)

func TestSummarizeWorkspaceSession(t *testing.T) {
	status := summarizeWorkspaceSession("/tmp/workspace", &team.SessionData{
		Rounds: 2,
		Tasks: []*team.TodoItem{
			{Status: team.TaskDone}, {Status: team.TaskError}, {Status: team.TaskBlocked},
			{Status: team.TaskSkipped}, {Status: team.TaskPending},
		},
	})
	if status.Total != 5 || status.Done != 1 || status.Error != 2 || status.Skipped != 1 || status.Pending != 1 {
		t.Errorf("unexpected status: %#v", status)
	}
}
