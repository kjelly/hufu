package tui

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// fakeChatRuntime records submitted commands and answers with a scripted Ack.
type fakeChatRuntime struct {
	mu       sync.Mutex
	commands []conversation.Command
	next     conversation.AckStatus
	reason   string
	turn     int
}

func (f *fakeChatRuntime) submit(cmd conversation.Command) conversation.Ack {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	status := f.next
	if status == "" {
		status = conversation.AckAccepted
	}
	ack := conversation.Ack{CommandID: cmd.ID, Status: status, Reason: f.reason}
	switch {
	case cmd.Kind == conversation.CommandSubmitPrompt && (status == conversation.AckAccepted || status == conversation.AckQueued):
		f.turn++
		ack.TurnID = fmt.Sprintf("turn-%d", f.turn)
		if status == conversation.AckQueued {
			ack.Position = 1
		}
	case (cmd.Kind == conversation.CommandCancelTurn || cmd.Kind == conversation.CommandWrapUp) && status == conversation.AckAccepted:
		// Like the controller, name the turn the control applies to.
		ack.TurnID = fmt.Sprintf("turn-%d", f.turn)
	}
	return ack
}

func newTestChat(t *testing.T, width, height int) (ChatModel, *fakeChatRuntime) {
	t.Helper()
	t.Setenv("NO_SPINNER", "1")
	runtime := &fakeChatRuntime{}
	m := NewChatModel(ChatOptions{
		Presentation: Options{Theme: ThemeDark, DisplayPreset: DisplayDefault},
		TeamInfo:     TeamInfo{TeamName: "alpha", Workspace: "/tmp/ws/alpha", Agents: []AgentInfoEntry{{Name: "coder", Model: "local/qwen"}, {Name: "reviewer"}}},
		Actions:      ChatActions{Submit: runtime.submit},
	})
	m = chatStep(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	return m, runtime
}

// chatStep applies one message and returns the next model. Commands are
// not executed.
func chatStep(t *testing.T, m ChatModel, msg tea.Msg) ChatModel {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(ChatModel)
}

// chatSubmit types text, presses Enter, and delivers the Ack.
func chatSubmit(t *testing.T, m ChatModel, text string) ChatModel {
	t.Helper()
	m.composer.SetValue(text)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(ChatModel)
	if cmd == nil {
		return m
	}
	msg := cmd()
	if _, ok := msg.(chatAckMsg); !ok {
		t.Fatalf("enter produced %T, want chatAckMsg", msg)
	}
	return chatStep(t, m, msg)
}

func chatEvent(t *testing.T, m ChatModel, ev conversation.Event) ChatModel {
	t.Helper()
	return chatStep(t, m, ChatEventMsg{Event: ev})
}

func plainView(m ChatModel) string { return ansi.Strip(m.render()) }

func TestChatSubmitKeepsDraftUntilAdmitted(t *testing.T) {
	tests := []struct {
		name      string
		status    conversation.AckStatus
		reason    string
		wantDraft string
		wantUser  bool
		wantPhase chatPhase
	}{
		{name: "accepted", status: conversation.AckAccepted, wantDraft: "", wantUser: true, wantPhase: phaseRunning},
		{name: "queued", status: conversation.AckQueued, wantDraft: "", wantUser: true, wantPhase: phaseIdle},
		{name: "busy", status: conversation.AckBusy, reason: "prompt queue is full", wantDraft: "fix the bug", wantPhase: phaseIdle},
		{name: "rejected", status: conversation.AckRejected, reason: "session closing", wantDraft: "fix the bug", wantPhase: phaseIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, runtime := newTestChat(t, 100, 30)
			runtime.next, runtime.reason = tt.status, tt.reason
			m.composer.SetValue("fix the bug")
			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(ChatModel)
			if len(runtime.commands) != 0 {
				t.Fatal("Update submitted directly; submission must run in a command")
			}
			if m.composer.Value() != "fix the bug" {
				t.Fatalf("draft cleared before the Ack: %q", m.composer.Value())
			}
			m = chatStep(t, m, cmd())
			if got := m.composer.Value(); got != tt.wantDraft {
				t.Fatalf("draft = %q, want %q", got, tt.wantDraft)
			}
			if got := m.transcript.findTurnItem(itemUser, "turn-1") >= 0; got != tt.wantUser {
				t.Fatalf("user item present = %v, want %v", got, tt.wantUser)
			}
			if m.phase != tt.wantPhase {
				t.Fatalf("phase = %v, want %v", m.phase, tt.wantPhase)
			}
			if tt.reason != "" && !strings.Contains(m.notice, tt.reason) {
				t.Fatalf("notice = %q, want reason %q", m.notice, tt.reason)
			}
		})
	}
}

func TestParseChatInput(t *testing.T) {
	tests := []struct {
		in       string
		wantKind chatInputKind
		wantName string
		wantText string
	}{
		{in: "/help", wantKind: inputCommand, wantName: "help"},
		{in: "/theme dark", wantKind: inputCommand, wantName: "theme"},
		{in: "/nope", wantKind: inputUnknownCommand, wantName: "nope"},
		{in: "//help me", wantKind: inputPrompt, wantText: "/help me"},
		{in: "/home/x is broken", wantKind: inputPrompt, wantText: "/home/x is broken"},
		{in: "/Help", wantKind: inputPrompt, wantText: "/Help"},
		{in: "please /help", wantKind: inputPrompt, wantText: "please /help"},
		{in: "/help\nmore", wantKind: inputCommand, wantName: "help"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseChatInput(tt.in)
			if got.kind != tt.wantKind || got.name != tt.wantName || (tt.wantKind == inputPrompt && got.text != tt.wantText) {
				t.Fatalf("parseChatInput(%q) = %+v", tt.in, got)
			}
		})
	}
}

func TestChatUnknownCommandIsNotSent(t *testing.T) {
	m, runtime := newTestChat(t, 100, 30)
	m.composer.SetValue("/frobnicate")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(ChatModel)
	if cmd != nil || len(runtime.commands) != 0 {
		t.Fatal("unknown command was submitted")
	}
	if !strings.Contains(m.notice, "//frobnicate") || m.composer.Value() != "/frobnicate" {
		t.Fatalf("notice = %q draft = %q", m.notice, m.composer.Value())
	}
}

func TestCompleteChatToken(t *testing.T) {
	info := TeamInfo{Agents: []AgentInfoEntry{{Name: "coder"}, {Name: "coach"}}, AvailableTeams: []string{"ops"}}
	tests := []struct{ in, want string }{
		{in: "/he", want: "/help "},
		{in: "/t", want: "/t"}, // tasks and theme are ambiguous
		{in: "ask @cod", want: "ask @coder "},
		{in: "ask @co", want: "ask @co"},
		{in: "@op", want: "@ops "},
		{in: "plain text", want: "plain text"},
		{in: "x /he", want: "x /he"}, // commands only complete as the first token
	}
	for _, tt := range tests {
		if got, _ := completeChatToken(tt.in, info); got != tt.want {
			t.Errorf("completeChatToken(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestChatComposerKeepsLargeUnicodePaste(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	line := "繁體中文 emoji 🚀 寬字元 ｗｉｄｅ | a | b |\n"
	paste := strings.Repeat(line, 100*1024/len(line)+1)
	m = chatStep(t, m, tea.PasteMsg{Content: paste})
	if got := m.composer.Value(); got != paste {
		t.Fatalf("paste changed: got %d bytes, want %d", len(got), len(paste))
	}
	lines := strings.Repeat("x\n", maxComposerLines+1)
	m = chatStep(t, m, tea.PasteMsg{Content: lines})
	if !strings.Contains(m.notice, "paste refused") || m.composer.Value() != paste {
		t.Fatalf("oversized paste was not refused cleanly: notice=%q", m.notice)
	}
}

func TestChatNewlineKeysDoNotSubmit(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'j', Mod: tea.ModCtrl}, {Code: tea.KeyEnter, Mod: tea.ModAlt}, {Code: tea.KeyEnter, Mod: tea.ModShift}} {
		m, runtime := newTestChat(t, 100, 30)
		m.composer.SetValue("line one")
		next, cmd := m.Update(key)
		m = next.(ChatModel)
		if cmd != nil && len(runtime.commands) == 0 {
			if _, ok := cmd().(chatAckMsg); ok {
				t.Fatalf("%q submitted the message", key.String())
			}
		}
		if !strings.Contains(m.composer.Value(), "\n") {
			t.Fatalf("%q did not insert a newline: %q", key.String(), m.composer.Value())
		}
	}
}

func settle(t *testing.T, m ChatModel, turnID, answer string, result *team.RunResult, err error, cancelled bool) ChatModel {
	t.Helper()
	return chatEvent(t, m, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: turnID, Answer: answer, Result: result, Err: err, Cancelled: cancelled}})
}

func TestChatTenTurnsKeepTheirOwnAnswers(t *testing.T) {
	m, _ := newTestChat(t, 100, 40)
	for i := 1; i <= 10; i++ {
		turnID := fmt.Sprintf("turn-%d", i)
		m = chatSubmit(t, m, fmt.Sprintf("question %d", i))
		m = chatEvent(t, m, conversation.TurnStarted{TurnID: turnID, Prompt: fmt.Sprintf("question %d", i)})
		m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{{ID: fmt.Sprintf("t%d", i), Agent: "coder", Status: team.TaskInProgress}}})
		m = settle(t, m, turnID, fmt.Sprintf("answer %d", i), &team.RunResult{Outcome: team.RunOutcomeUnverified}, nil, false)
		if m.phase != phaseIdle {
			t.Fatalf("turn %d did not return to idle", i)
		}
	}
	for i := 1; i <= 10; i++ {
		turnID := fmt.Sprintf("turn-%d", i)
		for _, kind := range []chatItemKind{itemUser, itemProgress, itemAssistant} {
			if m.transcript.findTurnItem(kind, turnID) < 0 {
				t.Fatalf("turn %d missing item kind %d", i, kind)
			}
		}
		assistant := m.transcript.items[m.transcript.findTurnItem(itemAssistant, turnID)]
		if assistant.body != fmt.Sprintf("answer %d", i) || !strings.Contains(assistant.title, "unverified") {
			t.Fatalf("turn %d assistant = %+v", i, assistant)
		}
		card := m.transcript.items[m.transcript.findTurnItem(itemProgress, turnID)].card
		if len(card.rows) != 1 || card.rows[0].id != fmt.Sprintf("t%d", i) {
			t.Fatalf("turn %d card rows = %+v", i, card.rows)
		}
	}
	if !strings.Contains(plainView(m), "answer 10") {
		t.Fatal("latest answer not visible at the bottom")
	}
}

func TestChatFrozenCardIgnoresLateTaskUpdates(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{{ID: "a", Agent: "coder", Status: team.TaskInProgress}}})
	m = settle(t, m, "turn-1", "done", nil, nil, false)
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{{ID: "a", Agent: "coder", Status: team.TaskError}}})
	card := m.transcript.items[m.transcript.findTurnItem(itemProgress, "turn-1")].card
	if !card.frozen || card.rows[0].status != team.TaskInProgress {
		t.Fatalf("frozen card changed: %+v", card)
	}
}

func TestChatOutcomeLabelsUseOnlyRuntimeFacts(t *testing.T) {
	stale := &team.RunResult{Outcome: team.RunOutcomeCompleted, GoalSatisfied: true}
	tests := []struct {
		name      string
		result    *team.RunResult
		err       error
		cancelled bool
		want      string
	}{
		{name: "no result", want: "Coordinator · no result"},
		{name: "unverified", result: &team.RunResult{Outcome: team.RunOutcomeUnverified}, want: "Coordinator · unverified"},
		{name: "cancelled", err: errors.New("context canceled"), cancelled: true, want: "Turn cancelled."},
		{name: "error", err: errors.New("provider down"), want: "Error: provider down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestChat(t, 100, 30)
			m.lastResult = stale
			m = chatSubmit(t, m, "go")
			m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
			m = settle(t, m, "turn-1", "partial answer", tt.result, tt.err, tt.cancelled)
			if view := plainView(m); !strings.Contains(view, tt.want) {
				t.Fatalf("view missing %q:\n%s", tt.want, view)
			}
			if m.lastResult == stale {
				t.Fatal("a stale RunResult from an earlier turn was kept as this turn's outcome")
			}
			if strings.Contains(plainView(m), "Execution completed successfully") {
				t.Fatal("success was synthesized without a RunResult saying so")
			}
		})
	}
}

func TestChatSanitizesUntrustedText(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = settle(t, m, "turn-1", "safe\x1b[31mred\x1b]52;c;ZXZpbA==\x07text", nil, nil, false)
	view := m.render()
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "ZXZpbA==\x07") {
		t.Fatalf("control sequence reached the terminal: %q", view)
	}
	if !strings.Contains(ansi.Strip(view), "saferedtext") {
		t.Fatalf("sanitized text missing:\n%s", ansi.Strip(view))
	}
}

func TestChatScrollbackIsNotPulledToBottom(t *testing.T) {
	m, _ := newTestChat(t, 80, 20)
	for i := range 20 {
		m = chatEvent(t, m, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: fmt.Sprintf("old-%d", i), Answer: fmt.Sprintf("old answer %d", i)}})
	}
	m = chatStep(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = chatStep(t, m, keyPress("k"))
	m = chatStep(t, m, keyPress("k"))
	before := plainView(m)
	m = chatEvent(t, m, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "new", Answer: "brand new answer"}})
	after := plainView(m)
	if strings.Contains(after, "brand new answer") {
		t.Fatal("view jumped to new content while scrolled up")
	}
	if !m.transcript.unseen || !strings.Contains(after, "↓ new") {
		t.Fatal("new-content indicator missing")
	}
	if strings.Split(before, "\n")[3] != strings.Split(after, "\n")[3] {
		t.Fatal("scrolled content moved when new content arrived")
	}
	m = chatStep(t, m, keyPress("G"))
	if !strings.Contains(plainView(m), "brand new answer") {
		t.Fatal("G did not return to the newest content")
	}
}

func TestChatTasksViewToggleKeepsDraftAndScroll(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m.composer.SetValue("draft text")
	m = chatStep(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.view != viewTasks {
		t.Fatal("Ctrl+T did not open the tasks view")
	}
	for _, key := range []tea.KeyPressMsg{keyPress("q"), {Code: tea.KeyEscape}} {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%q in tasks view returned no command", key.String())
		}
		if _, ok := cmd().(embeddedExitMsg); !ok {
			t.Fatalf("%q in tasks view must hand control back, not quit", key.String())
		}
	}
	m = chatStep(t, m, embeddedExitMsg{})
	if m.view != viewChat || m.composer.Value() != "draft text" {
		t.Fatalf("view=%v draft=%q after returning", m.view, m.composer.Value())
	}
}

func TestChatCtrlCContract(t *testing.T) {
	t.Run("running turn is cancelled", func(t *testing.T) {
		m, runtime := newTestChat(t, 100, 30)
		m = chatSubmit(t, m, "go")
		next, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		m = next.(ChatModel)
		m = chatStep(t, m, cmd())
		last := runtime.commands[len(runtime.commands)-1]
		if last.Kind != conversation.CommandCancelTurn || m.phase != phaseCancelling {
			t.Fatalf("last command = %v phase = %v", last.Kind, m.phase)
		}
		m = chatStep(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if m.overlay != overlayConfirmExit {
			t.Fatal("second Ctrl+C while cancelling should ask to quit")
		}
	})
	t.Run("idle clears draft then confirms exit", func(t *testing.T) {
		m, _ := newTestChat(t, 100, 30)
		m.composer.SetValue("draft")
		m = chatStep(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if m.composer.Value() != "" || m.overlay != overlayNone {
			t.Fatal("first Ctrl+C should only clear the draft")
		}
		m = chatStep(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if m.overlay != overlayConfirmExit {
			t.Fatal("Ctrl+C on an empty composer should ask to quit")
		}
		next, cmd := m.Update(keyPress("y"))
		m = next.(ChatModel)
		if m.phase != phaseClosing || cmd == nil {
			t.Fatal("confirming did not start closing")
		}
	})
}

func TestChatLayoutFitsEveryTerminalSize(t *testing.T) {
	sizes := [][2]int{{120, 30}, {80, 24}, {40, 20}, {30, 10}, {20, 5}, {0, 0}}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, _ := newTestChat(t, size[0], size[1])
			m = chatSubmit(t, m, "a long question that needs to wrap across several narrow lines 寬字元")
			m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
			m = settle(t, m, "turn-1", "```go\n\tfunc main() { println(\"a very long line of code that must be hard wrapped\") }\n```", &team.RunResult{Outcome: team.RunOutcomePartial}, nil, false)
			view := m.render()
			if size[0] == 0 {
				if view == "" {
					t.Fatal("empty view for zero size")
				}
				return
			}
			lines := strings.Split(view, "\n")
			if len(lines) != size[1] {
				t.Fatalf("view has %d lines, want %d", len(lines), size[1])
			}
			for i, line := range lines {
				if w := ansi.StringWidth(line); w > size[0] {
					t.Fatalf("line %d is %d cells wide (> %d): %q", i, w, size[0], ansi.Strip(line))
				}
			}
			if !strings.Contains(ansi.Strip(view), "> ") {
				t.Fatal("composer prompt not visible")
			}
		})
	}
}

func TestChatEpaperSchedulesNoAnimation(t *testing.T) {
	t.Setenv("NO_SPINNER", "")
	m := NewChatModel(ChatOptions{Presentation: Options{Theme: ThemeMono, DisplayPreset: DisplayEpaper, Spinner: true}})
	if m.dash.spinnerEnabled {
		t.Fatal("spinner enabled in e-paper mode")
	}
	if cmd := m.Init(); cmd != nil {
		if msg := cmd(); msg != nil {
			if batch, ok := msg.(tea.BatchMsg); !ok || len(batch) > 0 {
				for _, c := range batch {
					if c != nil {
						t.Fatalf("e-paper Init scheduled a command: %T", c())
					}
				}
			}
		}
	}
	next, cmd := m.Update(TasksUpdatedMsg{Items: []*team.TodoItem{{ID: "a", Status: team.TaskInProgress}}})
	if cmd != nil {
		t.Fatal("task update scheduled a spinner tick in e-paper mode")
	}
	m = next.(ChatModel)
	if _, cmd := m.focusComposer(); cmd != nil {
		t.Fatal("focusing the composer scheduled a cursor blink in e-paper mode")
	}
}

func TestChatThemeSwitchKeepsState(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m.composer.SetValue("keep me")
	m = chatStep(t, m, keyPress("T"))
	if m.dash.themeMode != ThemeDark || m.composer.Value() != "keep meT" {
		t.Fatalf("T in the composer must type, theme=%s draft=%q", m.dash.themeMode, m.composer.Value())
	}
	m = chatStep(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = chatStep(t, m, keyPress("T"))
	if m.dash.themeMode != ThemeMono || m.composer.Value() != "keep meT" {
		t.Fatalf("theme=%s draft=%q after T in the transcript", m.dash.themeMode, m.composer.Value())
	}
}
