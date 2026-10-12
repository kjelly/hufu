package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// chatWithTurns runs two turns that each touch one task.
func chatWithTurns(t *testing.T) ChatModel {
	t.Helper()
	m, _ := newTestChat(t, 100, 40)
	m = chatSubmit(t, m, "first")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{{ID: "a", Agent: "coder", Status: team.TaskInProgress}}})
	m = settle(t, m, "turn-1", "one", &team.RunResult{Outcome: team.RunOutcomeUnverified}, nil, false)
	m = chatSubmit(t, m, "second")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-2"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{
		{ID: "a", Agent: "coder", Status: team.TaskInProgress},
		{ID: "b", Agent: "reviewer", Status: team.TaskDone},
	}})
	m = settle(t, m, "turn-2", "two", &team.RunResult{Outcome: team.RunOutcomeCompleted}, nil, false)
	return chatStep(t, m, keyPress("esc")) // focus the transcript
}

func TestChatSelectionOpensTaskDetail(t *testing.T) {
	m := chatWithTurns(t)
	m = chatStep(t, m, keyPress("n"))
	if m.selection == nil || m.selection.task != "b" {
		t.Fatalf("first selection = %+v, want newest row b", m.selection)
	}
	if !strings.Contains(plainView(m), "│›reviewer") {
		t.Fatalf("selected row not highlighted:\n%s", plainView(m))
	}
	m = chatStep(t, m, keyPress("p"))
	if m.selection == nil || m.selection.task != "a" {
		t.Fatalf("selection after p = %+v, want a", m.selection)
	}
	m = chatStep(t, m, keyPress("enter"))
	if m.view != viewTasks || !m.dash.inDetail || m.dash.detailID != "a" {
		t.Fatalf("enter did not open task a: view=%v detail=%v id=%q", m.view, m.dash.inDetail, m.dash.detailID)
	}
	m = chatStep(t, m, keyPress("ctrl+t"))
	if m.view != viewChat || m.selection == nil || m.selection.task != "a" {
		t.Fatal("returning to chat lost the selection")
	}
	m = chatStep(t, m, keyPress("esc"))
	if m.selection != nil {
		t.Fatal("esc did not clear the selection")
	}
}

func TestChatSelectionReportsNewerActivity(t *testing.T) {
	m := chatWithTurns(t)
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{
		{ID: "a", Agent: "coder", Status: team.TaskError},
		{ID: "b", Agent: "reviewer", Status: team.TaskDone},
	}})
	m = chatStep(t, m, keyPress("n"))
	m = chatStep(t, m, keyPress("p")) // row a of turn 2's card
	m = chatStep(t, m, keyPress("enter"))
	if !strings.Contains(m.notice, "newer activity") {
		t.Fatalf("notice = %q, want newer-activity warning", m.notice)
	}
}

func TestChatAskUserOverlayRoutesReplies(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m.composer.SetValue("keep this draft")
	m = chatStep(t, m, keyPress("ctrl+t"))

	first := make(chan string, 1)
	m = chatStep(t, m, AskUserMsg{Question: "Which branch?", Type: "free_text", ReplyCh: first})
	if m.view != viewChat || !m.dash.inAskUser {
		t.Fatal("ask_user did not surface in the chat view")
	}
	if !strings.Contains(plainView(m), "Which branch?") {
		t.Fatalf("question not shown:\n%s", plainView(m))
	}
	for _, r := range "main" {
		m = chatStep(t, m, keyPress(string(r)))
	}
	m = chatStep(t, m, keyPress("enter"))
	if got := <-first; !strings.Contains(got, `"main"`) {
		t.Fatalf("first reply = %s, want the typed answer", got)
	}
	if m.dash.inAskUser || m.composer.Value() != "keep this draft" {
		t.Fatalf("after reply: inAskUser=%v draft=%q", m.dash.inAskUser, m.composer.Value())
	}

	second := make(chan string, 1)
	m = chatStep(t, m, AskUserMsg{Question: "Proceed?", Type: "single_choice", Options: []AskUserOption{{Label: "yes"}, {Label: "no"}}, ReplyCh: second})
	m = chatStep(t, m, keyPress("enter"))
	if got := <-second; !strings.Contains(got, `"yes"`) {
		t.Fatalf("second reply = %s, want the first option", got)
	}
	select {
	case extra := <-first:
		t.Fatalf("second answer leaked into the first question: %s", extra)
	default:
	}

	third := make(chan string, 1)
	m = chatStep(t, m, AskUserMsg{Question: "Cancelled?", Type: "free_text", ReplyCh: third})
	m = chatStep(t, m, AskUserCancelMsg{})
	if m.dash.inAskUser {
		t.Fatal("AskUserCancelMsg did not close the dialog")
	}
}

func TestChatSanitizesTaskActivity(t *testing.T) {
	m, _ := newTestChat(t, 120, 30)
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{{ID: "a", Agent: "coder", Status: team.TaskInProgress}}})
	m = chatStep(t, m, TaskLogMsg{TodoID: "a", Line: "bash token=[REDACTED] \x1b]52;c;ZXZpbA==\x07\x1b[2Jdone"})
	view := m.View().Content
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("control sequence from task activity reached the view: %q", view)
	}
	if !strings.Contains(plainView(m), "token=[REDACTED]") {
		t.Fatalf("task activity summary missing:\n%s", plainView(m))
	}
}

func TestEmbeddedDashboardStillAttachesTerminals(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m.dash.tasks = []*team.TodoItem{{ID: "task-1", Status: team.TaskInProgress}}
	m.dash.teamInfo.HufuBinary = "hufu"
	m.dash.teamInfo.Workspace = "/tmp/workspace"
	m.dash.terminals["task-1"] = "term-1"
	m.dash = m.dash.openTaskDetail(m.dash.tasks[0])
	m.view = viewTasks
	_, cmd := m.Update(keyPress("t"))
	if cmd == nil {
		t.Fatal("terminal attach returned no process command in the embedded dashboard")
	}
	if _, ok := cmd().(tea.QuitMsg); ok {
		t.Fatal("terminal attach quit the program")
	}
}
