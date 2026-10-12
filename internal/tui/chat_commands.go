package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
)

// chatCommandSpec documents one slash command. The same table drives
// parsing, completion, /help, and the command palette.
type chatCommandSpec struct {
	name    string
	usage   string
	summary string
}

var chatCommands = []chatCommandSpec{
	{name: "help", usage: "/help", summary: "show keys and commands"},
	{name: "cancel", usage: "/cancel", summary: "cancel the running turn"},
	{name: "wrap", usage: "/wrap", summary: "ask the coordinator to wrap up the running turn"},
	{name: "tasks", usage: "/tasks", summary: "open the tasks view (Ctrl+T)"},
	{name: "theme", usage: "/theme [auto|light|dark|mono]", summary: "switch the color theme"},
	{name: "exit", usage: "/exit", summary: "leave the chat"},
	{name: "quit", usage: "/quit", summary: "leave the chat"},
}

func lookupChatCommand(name string) (chatCommandSpec, bool) {
	for _, spec := range chatCommands {
		if spec.name == name {
			return spec, true
		}
	}
	return chatCommandSpec{}, false
}

// runChatCommand executes a parsed slash command. Commands with runtime
// effects go through the controller; read-only ones only change the view.
func (m ChatModel) runChatCommand(in chatInput, draft string) (ChatModel, tea.Cmd) {
	switch in.name {
	case "help":
		m = m.clearComposer(draft)
		m.openTextOverlay("Help", m.helpText())
		return m, nil
	case "cancel":
		m = m.clearComposer(draft)
		return m.sendControl(conversation.CommandCancelTurn, "")
	case "wrap":
		m = m.clearComposer(draft)
		return m.sendControl(conversation.CommandWrapUp, "")
	case "tasks":
		m = m.clearComposer(draft)
		m.view = viewTasks
		return m, nil
	case "theme":
		return m.themeCommand(in.arg, draft)
	case "exit", "quit":
		m = m.clearComposer(draft)
		return m.requestExit()
	}
	m.notice = fmt.Sprintf("/%s is not available", in.name)
	return m, nil
}

func (m ChatModel) themeCommand(arg, draft string) (ChatModel, tea.Cmd) {
	if arg == "" {
		m = m.clearComposer(draft)
		return m.cycleTheme()
	}
	mode, err := ParseThemeMode(arg)
	if err != nil {
		m.notice = err.Error()
		return m, nil
	}
	m = m.clearComposer(draft)
	m.dash.themeMode = mode
	m.dash.effectiveTheme = effectiveTheme(mode)
	m.dash.styles = newStyleSet(mode, m.dash.noColor)
	m.dash.themeGeneration++
	m.notice = "Theme: " + string(mode)
	return m.afterThemeChange()
}

func (m ChatModel) helpText() string {
	var b strings.Builder
	b.WriteString("Keys\n")
	for _, line := range []string{
		"Enter            send the message",
		"Ctrl+J, Alt+Enter insert a newline",
		"Tab              complete /commands and @names",
		"Esc              move focus to the transcript",
		"j/k, PgUp/PgDn   scroll (transcript focus); g/G top/bottom",
		"n/p, Enter       select a task row in a progress card; open its detail",
		"i, a             return to the composer",
		"T                cycle the theme (transcript focus)",
		"Ctrl+T           switch between chat and tasks",
		"Ctrl+C           cancel the turn / clear the draft / quit",
	} {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\nCommands\n")
	for _, spec := range chatCommands {
		fmt.Fprintf(&b, "  %-30s %s\n", spec.usage, spec.summary)
	}
	b.WriteString("\nPrefix a message with // to send a literal leading slash.")
	return b.String()
}
