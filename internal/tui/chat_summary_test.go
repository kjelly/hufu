package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// runTurnWithTasks starts turn-1, publishes tasks, and settles it.
func runTurnWithTasks(t *testing.T, m ChatModel, tasks []*team.TodoItem, answer string, outcome team.RunOutcome) ChatModel {
	t.Helper()
	m = chatSubmit(t, m, "review the last commits")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: tasks})
	return settle(t, m, "turn-1", answer, &team.RunResult{Outcome: outcome}, nil, false)
}

func TestTurnSummaryNamesFailedTaskBelowLongAnswer(t *testing.T) {
	m, _ := newTestChat(t, 120, 30)
	var tasks []*team.TodoItem
	for i := 1; i <= 18; i++ {
		tasks = append(tasks, &team.TodoItem{ID: fmt.Sprint(i), Agent: "reviewer", Status: team.TaskDone, Desc: "Review workset item"})
	}
	tasks = append(tasks, &team.TodoItem{ID: "19", Agent: "reviewer", Status: team.TaskError, ContractID: "inventory-review-handoffs", Desc: "Validate every review source"})
	longAnswer := strings.Repeat("results of a completed task\n", 80) + "FAILED: failure task_id=19"
	m = runTurnWithTasks(t, m, tasks, longAnswer, team.RunOutcomePartial)

	view := plainView(m)
	want := "Turn 1 · partial · 18 done · 1 error — failed: #19 inventory-review-handoffs (reviewer)"
	if !strings.Contains(view, want) {
		t.Fatalf("summary not visible at the bottom; want %q in:\n%s", want, view)
	}
	if strings.Contains(view, "┌ Turn 1") {
		t.Fatal("fixture precondition: the long answer should push the card out of view")
	}
	last := m.transcript.items[len(m.transcript.items)-1]
	if last.kind != itemSummary || last.tag != "failed" {
		t.Fatalf("last item = %+v, want a failed turn summary", last)
	}
}

func TestTurnSummary(t *testing.T) {
	tests := []struct {
		name       string
		rows       []progressRow
		wantLine   string
		wantFailed bool
	}{
		{name: "no tasks", wantLine: ""},
		{name: "all done", rows: []progressRow{{id: "1", status: team.TaskDone}, {id: "2", status: team.TaskDone}}, wantLine: "Turn 2 · completed · 2 done"},
		{name: "description fallback", rows: []progressRow{{id: "7", agent: "critic", status: team.TaskBlocked, desc: "Adversarially review escalated documentation workset item unit-0001"}},
			wantLine: "Turn 2 · completed · 1 blocked — failed: #7 Adversarially review escalated documentation wo… (critic)", wantFailed: true},
		{name: "more than three failures", rows: []progressRow{
			{id: "1", status: team.TaskError}, {id: "2", status: team.TaskError}, {id: "3", status: team.TaskProtocolIncomplete},
			{id: "4", status: team.TaskError}, {id: "5", status: team.TaskDone},
		}, wantLine: "Turn 2 · completed · 1 done · 3 error · 1 protocol_incomplete — failed: #1, #2, #3, +1 more", wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, failed := turnSummary(&progressCard{number: 2, rows: tt.rows}, "completed")
			if line != tt.wantLine || failed != tt.wantFailed {
				t.Fatalf("turnSummary = %q, %v; want %q, %v", line, failed, tt.wantLine, tt.wantFailed)
			}
		})
	}
}

func TestTurnSummaryForSuccessfulTurnIsNotAnError(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = runTurnWithTasks(t, m, []*team.TodoItem{{ID: "1", Agent: "coder", Status: team.TaskDone}}, "all good", team.RunOutcomeCompleted)
	last := m.transcript.items[len(m.transcript.items)-1]
	if last.kind != itemSummary || last.tag == "failed" || !strings.Contains(plainView(m), "Turn 1 · completed · 1 done") {
		t.Fatalf("successful summary = %+v\n%s", last, plainView(m))
	}
}
