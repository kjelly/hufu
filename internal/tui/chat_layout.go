package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/team"
)

const chatSidebarWidth = 30

// Width breakpoints: a sidebar from 110 columns, the composer hint line
// from 60, and the header from 40; below that only the transcript, the
// composer, and a one-line status remain.
func (m ChatModel) showSidebar() bool { return m.width >= 110 && m.height >= 10 }
func (m ChatModel) showHeader() bool  { return m.width >= 40 && m.height >= 10 }
func (m ChatModel) showHint() bool    { return m.width >= 40 && m.height >= 8 }

func (m ChatModel) transcriptWidth() int {
	w := m.width
	if m.showSidebar() {
		w -= chatSidebarWidth + 1
	}
	return max(w, 1)
}

// composerMaxRows caps the composer's dynamic height by terminal height.
func (m ChatModel) composerMaxRows() int {
	switch {
	case m.height < 10:
		return 1
	case m.height < 20:
		return 3
	default:
		return 6
	}
}

// transcriptHeight is what remains after the header, composer block, and
// status line.
func (m ChatModel) transcriptHeight() int {
	used := 1 + m.composer.Height() + 1 // separator, composer, status
	if m.showHeader() {
		used++
	}
	if m.showHint() {
		used++
	}
	return max(m.height-used, 0)
}

// layoutComposer applies the terminal size to the composer; the textarea
// recomputes its dynamic height from its content.
func (m *ChatModel) layoutComposer() {
	m.composer.MaxHeight = m.composerMaxRows()
	if m.width > 0 {
		m.composer.SetWidth(m.width)
	}
}

// View declares the terminal state: the chat always runs on the alternate
// screen; mouse reporting is only requested while the tasks view has it on.
func (m ChatModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	if m.view == viewTasks && m.dash.mouseEnabled {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// render draws the active screen. ask_user always wins; the tasks view is
// the embedded dashboard; otherwise the chat layout or a chat overlay.
func (m ChatModel) render() string {
	if m.width <= 0 || m.height <= 0 {
		return "Initialising…"
	}
	if m.dash.inAskUser {
		return m.dash.askUserView()
	}
	if m.view == viewTasks {
		return m.dash.render()
	}
	switch m.overlay {
	case overlayText:
		return m.textOverlayView()
	case overlayConfirmExit:
		return m.confirmExitView()
	case overlayPalette:
		return m.paletteView()
	}
	return m.chatView()
}

func (m ChatModel) chatView() string {
	s := m.dash.styles
	var rows []string
	if m.showHeader() {
		rows = append(rows, s.header.Render(truncateCells(m.headerText(), m.width)))
	}
	th := m.transcriptHeight()
	var body []string
	if th > 0 {
		body = strings.Split(m.transcript.vp.View(), "\n")
		for len(body) < th {
			body = append(body, "")
		}
		body = body[:th]
	}
	if m.showSidebar() {
		side := m.sidebarLines(th)
		tw := m.transcriptWidth()
		for i := range body {
			body[i] = padCells(body[i], tw) + " " + side[i]
		}
	}
	rows = append(rows, body...)
	rows = append(rows, s.dim.Render(strings.Repeat("─", m.width)))
	rows = append(rows, strings.Split(m.composer.View(), "\n")...)
	if m.showHint() {
		rows = append(rows, m.hintLine())
	}
	rows = append(rows, s.dim.Render(truncateCells(m.statusText(), m.width)))
	return strings.Join(rows, "\n")
}

func padCells(line string, width int) string {
	line = truncateCells(line, width)
	if gap := width - ansi.StringWidth(line); gap > 0 {
		return line + strings.Repeat(" ", gap)
	}
	return line
}

func (m ChatModel) headerText() string {
	info := m.dash.teamInfo
	parts := []string{"hufu"}
	if info.TeamName != "" {
		parts = append(parts, sanitizeTerminalText(info.TeamName))
	}
	if info.Workspace != "" {
		parts = append(parts, "ws:"+sanitizeTerminalText(filepath.Base(info.Workspace)))
	}
	parts = append(parts, m.headerPhase())
	return " " + strings.Join(parts, " · ")
}

func (m ChatModel) phaseName() string {
	switch m.phase {
	case phaseRunning:
		return "running"
	case phaseCancelling:
		return "cancelling"
	case phaseClosing:
		return "closing"
	default:
		return "idle"
	}
}

func (m ChatModel) hintLine() string {
	s := m.dash.styles
	if m.notice != "" {
		return s.wrapUp.Render(truncateCells(m.notice, m.width))
	}
	hint := "Enter send · Ctrl+J newline · Tab complete · Esc scroll · Ctrl+T tasks · ? help"
	if m.focus == focusTranscript {
		hint = "j/k scroll · n/p select task · enter open · i compose · T theme · Ctrl+T tasks · ? help"
	}
	return s.dim.Render(truncateCells(hint, m.width))
}

func (m ChatModel) statusText() string {
	last := "none"
	if m.lastResult != nil {
		last = string(m.lastResult.Outcome)
	}
	parts := []string{
		fmt.Sprintf("queued:%d", m.queued),
		"last: " + last,
		"theme:" + string(m.dash.themeMode),
	}
	if activity := m.activityText(); activity != "" {
		parts = append([]string{"⟳ " + activity}, parts...)
	}
	if m.transcript.unseen {
		parts = append([]string{"↓ new"}, parts...)
	}
	if !m.showHint() && m.notice != "" {
		parts = append([]string{m.notice}, parts...)
	}
	return " " + strings.Join(parts, " · ")
}

func (m ChatModel) sidebarLines(height int) []string {
	s := m.dash.styles
	w := chatSidebarWidth
	var lines []string
	lines = append(lines, s.header.Render(truncateCells("Agents", w)))
	for _, agent := range m.dash.teamInfo.Agents {
		line := sanitizeTerminalText(agent.Name)
		if agent.Model != "" {
			line += " · " + sanitizeTerminalText(agent.Model)
		}
		lines = append(lines, truncateCells(line, w))
	}
	lines = append(lines, "", s.header.Render(truncateCells("Tasks", w)))
	counts := map[team.TaskStatus]int{}
	for _, task := range m.dash.tasks {
		if task != nil && task.ID != team.CoordTodoID {
			counts[task.Status]++
		}
	}
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, string(status))
	}
	sort.Strings(statuses)
	if len(statuses) == 0 {
		lines = append(lines, s.dim.Render("no tasks yet"))
	}
	for _, status := range statuses {
		lines = append(lines, truncateCells(fmt.Sprintf("%-20s %d", status, counts[team.TaskStatus(status)]), w))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines[:height]
}

func (m ChatModel) textOverlayView() string {
	s := m.dash.styles
	inner := max(min(m.width-8, 96), 10)
	var lines []string
	for _, line := range strings.Split(sanitizeTerminalText(m.overlayBody), "\n") {
		lines = append(lines, strings.Split(ansi.Hardwrap(line, inner, true), "\n")...)
	}
	visible := max(m.height-8, 1)
	start := min(m.overlayLines, max(len(lines)-visible, 0))
	end := min(start+visible, len(lines))
	content := s.header.Render(truncateCells(m.overlayTitle, inner)) + "\n\n" +
		strings.Join(lines[start:end], "\n") + "\n\n" +
		s.dim.Render(truncateCells("j/k scroll · esc close", inner))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s.infoBox.Render(content))
}

func (m ChatModel) confirmExitView() string {
	s := m.dash.styles
	var details []string
	switch m.phase {
	case phaseRunning, phaseCancelling:
		details = append(details, "The running turn will be cancelled.")
	}
	if m.queued > 0 {
		details = append(details, fmt.Sprintf("%d queued message(s) will be discarded.", m.queued))
	}
	content := s.bold.Render("Quit hufu chat?")
	if len(details) > 0 {
		content += "\n\n" + strings.Join(details, "\n")
	}
	content += "\n\n" + s.dim.Render("y/enter quit · n/esc stay")
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s.confirmBox.Render(content))
}
