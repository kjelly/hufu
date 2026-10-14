package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/kjelly/hufu/internal/team"
)

func TestResultIdentityUpdateIsPureAndPreservesTaskState(t *testing.T) {
	m := New("prompt", TeamInfo{TeamName: "review"})
	item := &team.TodoItem{ID: "1", Agent: "helper", Status: team.TaskInProgress}
	m.tasks = []*team.TodoItem{item}
	m.width = 40
	id := "res-" + strings.Repeat("a", 64)
	updated, _ := m.Update(TaskResultIdentityMsg{TodoID: "1", ResultID: id, Team: "review"})
	next := updated.(Model)
	if len(m.resultIdentities) != 0 {
		t.Fatal("Update mutated prior model identity map")
	}
	if item.Status != team.TaskInProgress {
		t.Fatal("identity message changed task completion status")
	}
	for _, line := range next.resultIdentityLines("1") {
		for display := range strings.SplitSeq(line, "\n") {
			if lipgloss.Width(display) > 40 {
				t.Fatalf("identity line exceeds width: %s", display)
			}
		}
	}
	header := next.renderDetailHeader(item)
	if !strings.Contains(header, "Result ID:") || !strings.Contains(header, "review") || !strings.Contains(header, "hufu results show") {
		t.Fatalf("missing result reference: %s", header)
	}
}

func TestResultIdentityIgnoresNonResultIDs(t *testing.T) {
	m := New("prompt", TeamInfo{})
	updated, _ := m.Update(TaskResultIdentityMsg{TodoID: "1", ResultID: "task-1", Team: "review"})
	if len(updated.(Model).resultIdentities) != 0 {
		t.Fatal("invalid ID was accepted")
	}
}
