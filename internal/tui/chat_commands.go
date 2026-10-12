package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
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
	{name: "agents", usage: "/agents", summary: "list the team's agents"},
	{name: "skills", usage: "/skills", summary: "list loaded skills"},
	{name: "config", usage: "/config", summary: "show team, workspace, and model"},
	{name: "status", usage: "/status", summary: "show the last turn's canonical outcome"},
	{name: "tasks", usage: "/tasks", summary: "open the tasks view (Ctrl+T)"},
	{name: "cost", usage: "/cost", summary: "show the last run's generation cost"},
	{name: "theme", usage: "/theme [auto|light|dark|mono]", summary: "switch the color theme"},
	{name: "save", usage: "/save <path>", summary: "export the conversation as Markdown"},
	{name: "cancel", usage: "/cancel", summary: "cancel the running turn"},
	{name: "wrap", usage: "/wrap", summary: "ask the coordinator to wrap up the running turn"},
	{name: "reset", usage: "/reset", summary: "clear the conversation context"},
	{name: "team", usage: "/team [name]", summary: "list teams, or switch to another team"},
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
// effects go through the controller or a ChatActions call in a tea.Cmd;
// read-only ones only change the view.
func (m ChatModel) runChatCommand(in chatInput, draft string) (ChatModel, tea.Cmd) {
	m = m.clearComposer(draft)
	switch in.name {
	case "help":
		m.openTextOverlay("Help", m.helpText())
	case "agents":
		m.openTextOverlay("Agents", m.agentsText())
	case "skills":
		m.openTextOverlay("Skills", listOrNone(m.dash.teamInfo.Skills, "(no skills loaded)"))
	case "config":
		info := m.dash.teamInfo
		m.openTextOverlay("Config", fmt.Sprintf("Team: %s\nWorkspace: %s\nModel: %s", info.TeamName, info.Workspace, info.DefaultModel))
	case "status":
		m.openTextOverlay("Status", m.statusReport())
	case "tasks":
		m.view = viewTasks
	case "cost":
		return m.costCommand()
	case "theme":
		return m.themeCommand(in.arg)
	case "save":
		return m.saveCommand(in.arg)
	case "cancel":
		return m.sendControl(conversation.CommandCancelTurn, "")
	case "wrap":
		return m.sendControl(conversation.CommandWrapUp, "")
	case "reset":
		return m.sendControl(conversation.CommandResetContext, "")
	case "team":
		if in.arg == "" {
			m.openTextOverlay("Teams", listOrNone(m.dash.teamInfo.AvailableTeams, "(team switching is unavailable with --default)"))
			return m, nil
		}
		return m.sendControl(conversation.CommandSwitchTeam, in.arg)
	case "exit", "quit":
		return m.requestExit()
	default:
		m.notice = fmt.Sprintf("/%s is not available", in.name)
	}
	return m, nil
}

func listOrNone(values []string, none string) string {
	if len(values) == 0 {
		return none
	}
	return strings.Join(values, "\n")
}

func (m ChatModel) agentsText() string {
	var lines []string
	for _, agent := range m.dash.teamInfo.Agents {
		lines = append(lines, fmt.Sprintf("@%s [%s] %s", agent.Name, agent.Role, agent.Model))
	}
	return listOrNone(lines, "(no agents)")
}

// statusReport describes the last settled turn from runtime facts only.
func (m ChatModel) statusReport() string {
	outcome := "no run yet"
	if m.lastResult != nil {
		outcome = team.FormatCanonicalStatus(m.lastResult)
	}
	runs := "none"
	if len(m.lastRunIDs) > 0 {
		runs = strings.Join(m.lastRunIDs, ", ")
	}
	turn := m.lastTurnID
	if turn == "" {
		turn = "none"
	}
	return fmt.Sprintf("State: %s\nQueued: %d\nLast turn: %s\nRun IDs: %s\nOutcome: %s", m.phaseName(), m.queued, turn, runs, outcome)
}

// lastRunID is the run the cost and status commands describe.
func (m ChatModel) lastRunID() string {
	if m.lastResult != nil && m.lastResult.RunID != "" {
		return m.lastResult.RunID
	}
	if len(m.lastRunIDs) > 0 {
		return m.lastRunIDs[len(m.lastRunIDs)-1]
	}
	return ""
}

func (m ChatModel) costCommand() (ChatModel, tea.Cmd) {
	runID := m.lastRunID()
	cost := m.actions.Cost
	switch {
	case runID == "":
		m.notice = "no run yet"
		return m, nil
	case cost == nil:
		m.notice = "/cost is unavailable"
		return m, nil
	}
	return m, func() tea.Msg {
		body, err := cost(runID)
		return chatCommandResultMsg{title: "Cost · " + runID, body: body, err: err}
	}
}

func (m ChatModel) saveCommand(path string) (ChatModel, tea.Cmd) {
	save := m.actions.Save
	switch {
	case path == "":
		m.notice = "usage: /save <path>"
		return m, nil
	case save == nil:
		m.notice = "/save is unavailable"
		return m, nil
	}
	return m, func() tea.Msg {
		if err := save(path); err != nil {
			return chatCommandResultMsg{title: "/save", err: err}
		}
		return chatCommandResultMsg{title: "Saved conversation to " + path}
	}
}

func (m ChatModel) themeCommand(arg string) (ChatModel, tea.Cmd) {
	if arg == "" {
		return m.cycleTheme()
	}
	mode, err := ParseThemeMode(arg)
	if err != nil {
		m.notice = err.Error()
		return m, nil
	}
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
		"Ctrl+J, Alt+Enter, Shift+Enter  insert a newline",
		"Tab              complete /commands and @names",
		"Esc              move focus to the transcript",
		"j/k, PgUp/PgDn   scroll (transcript focus); g/G top/bottom",
		"n/p, Enter       select a task row in a progress card; open its detail",
		"i, a             return to the composer",
		"T                cycle the theme (transcript focus)",
		"Ctrl+T           switch between chat and tasks",
		"Ctrl+P           command palette",
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
