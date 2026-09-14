package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func (m Model) View() string {
	if m.width == 0 {
		return "Initialising…"
	}
	// Overlay dispatch: switch on the active overlay instead of a chain
	// of bool checks. The priority order is centralized in
	// currentOverlay() (see overlay.go), so adding a new overlay only
	// requires extending the enum and currentOverlay().
	switch m.currentOverlay() {
	case OverlayAskUser:
		return m.askUserView()
	case OverlayHelp:
		return m.helpView()
	case OverlayInfo:
		return m.infoPanelView()
	case OverlaySearch:
		return m.searchView()
	case OverlayPromptInput:
		return m.promptInputView()
	case OverlayConfirm:
		return m.confirmView()
	case OverlayDetail:
		return m.detailView()
	case OverlayResult:
		return m.resultView()
	case OverlayActivityLog:
		return m.activityLogView()
	case OverlayMemory:
		return m.memoryView()
	case OverlayOperator:
		return m.operatorView()
	}
	return m.columnsView()
}

func (m Model) resultView() string {
	header := m.styles.header.Render("Result") + "\n" + m.styles.dim.Render("esc/enter back · q quit") + "\n\n"
	return header + m.vp.View()
}

// ── Column view ───────────────────────────────────────────────────────────────

var colTitles = [6]string{"PENDING", "PLANNED", "IN PROGRESS", "DONE", "SKIP", "ERROR"}

func (m Model) renderPromptWidget(w int) string {
	innerW := w - 4
	if innerW < 4 {
		innerW = 4
	}
	labelPlain := "Task  "
	labelRunes := len([]rune(labelPlain))
	textW := innerW - labelRunes
	if textW < 1 {
		textW = 1
	}
	wrapped := wrapCells(m.prompt, textW, 5)
	var content strings.Builder
	content.WriteString(m.styles.prompt.Render(labelPlain))
	if len(wrapped.Lines) > 0 {
		content.WriteString(wrapped.Lines[0])
		for _, line := range wrapped.Lines[1:] {
			content.WriteString("\n")
			content.WriteString(strings.Repeat(" ", labelRunes))
			content.WriteString(line)
		}
	}
	return m.styles.promptBox.Width(innerW).Render(content.String())
}

func (m Model) promptWidgetHeight() int {
	if m.prompt == "" {
		return 3
	}
	innerW := m.width - 4
	if innerW < 4 {
		innerW = 4
	}
	labelRunes := len([]rune("Task  "))
	textW := innerW - labelRunes
	if textW < 1 {
		textW = 1
	}
	wrapped := wrapCells(m.prompt, textW, 5)
	lines := len(wrapped.Lines)
	if lines == 0 {
		return 3
	}
	return lines + 2 // +2 for border top+bottom
}

const maxResultLines = 8
const maxFeedLines = 6
const maxTaskLogLines = 5000

// statusAreaHeight returns the number of terminal lines occupied by the status area.
func (m Model) statusAreaHeight() int {
	if m.finished && m.result != "" {
		raw := strings.TrimSpace(m.result)
		n := strings.Count(raw, "\n") + 1
		if n > maxResultLines {
			n = maxResultLines + 1 // +1 for the truncation indicator line
		}
		return n + 2 // top + bottom border
	}
	return 1
}

// countFeedLines returns the actual number of terminal lines used by the activity feed.
func (m Model) countFeedLines() int {
	n := 0
	for _, entry := range m.recentLogs {
		n += strings.Count(entry, "\n") + 1
	}
	if n > maxFeedLines {
		return maxFeedLines
	}
	return n
}

// renderStatusArea renders the status bar or result box.
func (m Model) renderStatusArea(w int) string {
	if m.finished && m.result != "" {
		innerW := w - 4 // border(1) + padding(1) each side
		if innerW < 4 {
			innerW = 4
		}
		raw := strings.TrimSpace(m.result)
		lines := strings.Split(raw, "\n")
		if len(lines) > maxResultLines {
			remaining := len(lines) - maxResultLines
			lines = lines[:maxResultLines]
			lines = append(lines, m.styles.dim.Render(fmt.Sprintf("... (%d more lines)", remaining)))
		}
		labelPlain := "Result  "
		var sb strings.Builder
		sb.WriteString(m.styles.resultLabel.Render(labelPlain))
		for i, l := range lines {
			if i == 0 {
				sb.WriteString(truncateCells(l, innerW-len([]rune(labelPlain))))
			} else {
				sb.WriteString("\n")
				sb.WriteString(truncateCells(l, innerW))
			}
		}
		return m.styles.resultBox.Width(innerW).Render(sb.String())
	}

	text := m.statusText
	// Prefer showing the most recent in_progress task's name so the status
	// bar reflects what is happening *now*, not whatever event arrived last
	// (which may be for a task that has already completed in the background).
	if !m.finished {
		for i := len(m.tasks) - 1; i >= 0; i-- {
			if t := m.tasks[i]; t != nil && t.Status == team.TaskInProgress {
				text = "  " + m.styles.agent.Render(t.Agent) + m.styles.dim.Render("  "+t.Desc)
				return truncateLineCells(ansi.Strip(text), m.width-3)
			}
		}
	}
	if text == "" {
		if m.finished {
			return m.styles.dim.Render("  ✓ Done")
		}
		return m.styles.dim.Render("  ⟳ Initialising…")
	}
	if m.spinnerActive() {
		text = spinnerFrames[m.spinnerFrame] + " " + text
	}
	// Strip ANSI, take the last (most recent) line, and truncate to terminal
	// width so the status bar always occupies exactly 1 line.
	plain := ansi.Strip(text)
	if idx := strings.LastIndexByte(plain, '\n'); idx >= 0 {
		plain = plain[idx+1:]
	}
	maxW := m.width - 3
	if maxW < 1 {
		maxW = 1
	}
	plain = truncateLineCells(plain, maxW)
	return "  " + plain
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m Model) spinnerActive() bool {
	if !m.spinnerEnabled || m.finished {
		return false
	}
	for _, task := range m.tasks {
		if task.Status == team.TaskInProgress {
			return true
		}
	}
	return false
}

func (m Model) spinnerTickCmd() tea.Cmd {
	if !m.spinnerActive() {
		return nil
	}
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

func (m Model) renderActivityFeed(w int) string {
	if len(m.recentLogs) == 0 {
		return ""
	}
	feedW := w - 2
	var b strings.Builder
	lineCount := 0
	for _, entry := range m.recentLogs {
		for _, line := range strings.Split(entry, "\n") {
			if lineCount >= maxFeedLines {
				break
			}
			runes := []rune(line)
			if len(runes) > feedW {
				line = string(runes[:feedW-1]) + "…"
			}
			b.WriteString("  ")
			b.WriteString(m.styles.dim.Render(line))
			b.WriteString("\n")
			lineCount++
		}
		if lineCount >= maxFeedLines {
			break
		}
	}
	return b.String()
}

func (m Model) columnsView() string {
	w := m.width
	if w < 9 {
		return ""
	}
	summary := m.renderSummaryStrip(w)
	if w < 60 {
		warning := m.styles.infoBox.Render("Terminal too narrow\n\nResize to at least 60 columns to view task status.")
		return summary + "\n" + lipgloss.Place(w, max(m.height-lipgloss.Height(summary)-1, 1), lipgloss.Center, lipgloss.Center, warning)
	}

	widget := m.renderPromptWidget(w)
	progress := m.renderProgressBar(w)
	statusArea := m.renderStatusArea(w)
	activityFeed := m.renderActivityFeed(w)
	statusH := m.statusAreaHeight()
	promptH := m.promptWidgetHeight()
	feedH := m.countFeedLines()
	summaryH := lipgloss.Height(summary)

	// promptH + blank + statusH + blank + feedH + blank + blank + footer
	// feedH already includes the separator: 0 when empty, feedH+1 when non-empty.
	feedTotal := 0
	if feedH > 0 {
		feedTotal = feedH + 1
	}
	progressH := 0
	if progress != "" {
		progressH = 1
	}
	bodyH := m.height - summaryH - 1 - promptH - 1 - progressH - statusH - 1 - feedTotal - 2
	if bodyH < 2 {
		bodyH = 2
	}

	if m.isCompact() {
		return m.compactColumnsView(widget, progress, statusArea, activityFeed, bodyH, feedH)
	}

	// Five │ dividers, so each of six columns = (w-5)/6.
	colW := (w - 5) / 6

	c0 := m.renderCol(0, colW, bodyH)
	c1 := m.renderCol(1, colW, bodyH)
	c2 := m.renderCol(2, colW, bodyH)
	c3 := m.renderCol(3, colW, bodyH)
	c4 := m.renderCol(4, colW, bodyH)
	c5 := m.renderCol(5, colW, bodyH)
	div := m.styles.dim.Render("│")

	body := lipgloss.JoinHorizontal(lipgloss.Top, c0, div, c1, div, c2, div, c3, div, c4, div, c5)
	prefix := summary + "\n" + widget + "\n"
	if progress != "" {
		prefix += progress + "\n"
	}
	if feedH > 0 {
		return prefix + statusArea + "\n" + activityFeed + "\n" + body + "\n\n" + m.footer()
	}
	return prefix + statusArea + "\n" + body + "\n\n" + m.footer()
}

func (m Model) isCompact() bool {
	return m.forceCompact || (m.width >= 60 && m.width < 100)
}

func (m Model) compactColumnsView(widget, progress, statusArea, activityFeed string, bodyH, feedH int) string {
	groups := []struct {
		title string
		cols  []int
	}{
		{title: "Queued", cols: []int{0, 1}},
		{title: "Active", cols: []int{2}},
		{title: "Finished", cols: []int{3, 4, 5}},
	}
	colW := (m.width - 2) / len(groups)
	div := m.styles.dim.Render("│")
	columns := make([]string, 0, len(groups))
	for _, group := range groups {
		columns = append(columns, m.renderCompactCol(group.title, group.cols, colW, bodyH))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, columns[0], div, columns[1], div, columns[2])
	prefix := m.renderSummaryStrip(m.width) + "\n" + widget + "\n"
	if progress != "" {
		prefix += progress + "\n"
	}
	if feedH > 0 {
		return prefix + statusArea + "\n" + activityFeed + "\n" + body + "\n\n" + m.footer()
	}
	return prefix + statusArea + "\n" + body + "\n\n" + m.footer()
}

func (m Model) renderSummaryStrip(width int) string {
	summary := m.operatorSummary
	if !m.hasOperatorSummary {
		summary = m.liveSummary()
	}
	inner := max(width-4, 8)
	lines := []string{
		m.styles.bold.Render("State: ") + truncateCells(summary.State, inner-7),
		m.styles.bold.Render("What:  ") + truncateCells(summary.What, inner-7),
		m.styles.bold.Render("Next:  ") + truncateCells(summary.Next, inner-7),
	}
	if width >= 100 {
		teamName, workspace := m.teamInfo.TeamName, m.teamInfo.Workspace
		runID, branchID := "unknown", "unknown"
		if m.hasOperatorSummary {
			teamName, workspace = m.operatorScope.TeamName, m.operatorScope.WorkspaceExact
			runID, branchID = m.operatorScope.RunID, m.operatorScope.BranchID
		}
		scope := fmt.Sprintf("Team: %s · Workspace: %s · Run: %s · Branch: %s", valueOrUnknown(teamName), valueOrUnknown(workspace), valueOrUnknown(runID), valueOrUnknown(branchID))
		lines = append([]string{m.styles.header.Render(truncateCells(scope, inner))}, lines...)
	}
	if width >= 80 && summary.Data != "" {
		lines = append(lines, m.styles.dim.Render("Data:  "+truncateCells(summary.Data, inner-7)))
	}
	return m.styles.promptBox.Width(inner).Render(strings.Join(lines, "\n"))
}

func (m Model) liveSummary() operator.OperatorSummary {
	state := "READY"
	what := "No task is currently active."
	next := "No operator action is required."
	if m.inAskUser {
		return operator.OperatorSummary{State: "WAITING_INPUT", What: "The runtime is waiting at an explicit input gate.", Next: "Answer the open prompt or cancel it explicitly."}
	}
	for _, task := range m.tasks {
		switch task.Status {
		case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
			state, what, next = "BLOCKED", "A task requires review; open its detail for canonical failure information.", "Inspect the blocked task before retrying."
			return operator.OperatorSummary{State: state, What: what, Next: next}
		case team.TaskVerifying:
			state, what, next = "VERIFYING", "Objective verification is in progress.", "Wait for verification; do not retry active work."
		case team.TaskInProgress:
			if state != "VERIFYING" {
				state, what, next = "EXECUTING", task.Agent+" is working on "+task.Desc, "Wait for the runtime; use Enter to inspect details."
			}
		}
	}
	if m.finished {
		state, what, next = "FINISHED", team.FormatCanonicalStatus(m.runResult), "Review the result; press q to close this owner view."
	}
	return operator.OperatorSummary{
		State: operator.SafeDisplayText(state, 80), What: operator.SafeDisplayText(what, 320),
		Next: operator.SafeDisplayText(next, 320), Data: "live owner view; persisted freshness is not yet available",
	}
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return sanitizeTerminalText(value)
}

func truncateCells(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(sanitizeTerminalText(value), width, "…")
}

func (m Model) renderProgressBar(width int) string {
	if len(m.tasks) < 3 {
		return ""
	}
	var done, active, failed int
	for _, task := range m.tasks {
		switch task.Status {
		case team.TaskDone:
			done++
		case team.TaskInProgress, team.TaskPaused, team.TaskVerifying:
			active++
		case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
			failed++
		}
	}
	barWidth := 20
	if width < 55 {
		barWidth = 10
	}
	filled := done * barWidth / len(m.tasks)
	bar := m.styles.doneIcon.Render(strings.Repeat("█", filled)) + m.styles.dim.Render(strings.Repeat("░", barWidth-filled))
	return fmt.Sprintf("  %s  %d/%d tasks · %d active · %d errors", bar, done, len(m.tasks), active, failed)
}

func (m Model) renderCompactCol(title string, cols []int, width, height int) string {
	focused := false
	var items []*team.TodoItem
	for _, col := range cols {
		if m.col == col {
			focused = true
		}
		items = append(items, m.colItems(col)...)
	}

	plainTitle := fmt.Sprintf("%s (%d)", title, len(items))
	var sb strings.Builder
	if focused {
		sb.WriteString(m.styles.header.Render(truncateLineCells(plainTitle, width)) + "\n")
	} else {
		sb.WriteString(m.styles.dim.Render(truncateLineCells(plainTitle, width)) + "\n")
	}
	sb.WriteString("\n")
	usedLines := 2
	selectedID := ""
	if focused {
		selected := m.colItems(m.col)
		if m.row >= 0 && m.row < len(selected) {
			selectedID = selected[m.row].ID
		}
	}
	for _, item := range items {
		if usedLines >= height {
			break
		}
		for _, line := range m.itemLines(item, item.ID == selectedID, m.isCurrentSearchMatch(item.ID), width) {
			if usedLines >= height {
				break
			}
			sb.WriteString(line + "\n")
			usedLines++
		}
		if usedLines < height {
			sb.WriteString("\n")
			usedLines++
		}
	}
	for usedLines < height {
		sb.WriteString("\n")
		usedLines++
	}
	return sb.String()
}

func (m *Model) trimTaskLogs(todoID string) {
	lines := m.logs[todoID]
	if len(lines) <= maxTaskLogLines {
		return
	}
	dropped := len(lines) - maxTaskLogLines
	m.logs[todoID] = lines[dropped:]
	if m.detailID == todoID {
		m.cursorLine = max(0, m.cursorLine-dropped)
		m.visualStart = max(0, m.visualStart-dropped)
		m.visualEnd = max(0, m.visualEnd-dropped)
	}
}

func (m Model) renderCol(col, width, height int) string {
	focused := col == m.col
	items := m.colItems(col)

	titleLabel := colTitles[col]
	count := fmt.Sprintf("(%d)", len(items))
	plainTitle := titleLabel + " " + count
	truncated := truncateLineCells(plainTitle, width)
	var sb strings.Builder
	if focused {
		sb.WriteString(m.styles.header.Render(truncated) + "\n")
	} else {
		sb.WriteString(m.styles.dim.Render(truncated) + "\n")
	}
	sb.WriteString("\n")
	usedLines := 2

	start := m.scrollOff[col]
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}

	for i := start; i < len(items); i++ {
		if usedLines >= height {
			break
		}
		selected := focused && i == m.row
		isMatch := m.isCurrentSearchMatch(items[i].ID)
		for _, l := range m.itemLines(items[i], selected, isMatch, width) {
			if usedLines >= height {
				break
			}
			sb.WriteString(l + "\n")
			usedLines++
		}
		if i < len(items)-1 && usedLines < height {
			sb.WriteString("\n")
			usedLines++
		}
	}

	for usedLines < height {
		sb.WriteString("\n")
		usedLines++
	}
	return sb.String()
}

func (m Model) isCurrentSearchMatch(id string) bool {
	if len(m.searchResults) == 0 {
		return false
	}
	if m.searchIdx >= len(m.searchResults) {
		return false
	}
	return m.searchResults[m.searchIdx].ID == id
}

const maxDescLines = 5

func (m Model) itemLines(item *team.TodoItem, selected bool, isMatch bool, width int) []string {
	icon, iconSt := m.taskIconStyle(item.Status)
	sourceLabel := m.sourceTag(item.Source)
	agentLine := truncateLineCells(item.Agent, width-3-lipgloss.Width(sourceLabel))

	if item.ID == team.CoordTodoID {
		coordLabel := m.styles.dim.Render("(coordinator)")
		agentLine = truncateLineCells(item.Agent, width-len("(coordinator)")-4)

		var lines []string
		if selected {
			line1 := "▶ " + m.styles.agent.Render(agentLine) + " " + coordLabel
			lines = []string{line1}
			lines = append(lines, m.formatDescLines("coordinating", width, 1, selected)...)
		} else {
			line1 := iconSt.Render(icon+" ") + m.styles.agent.Render(agentLine) + " " + coordLabel
			lines = []string{line1}
			lines = append(lines, m.formatDescLines("coordinating", width, 1, false)...)
		}
		if selected {
			styledLines := make([]string, len(lines))
			for i, l := range lines {
				styledLines[i] = m.styles.selectedBG.Width(width).Render(m.styles.selectedFG.Render(l))
			}
			return styledLines
		}
		return lines
	}

	var lines []string
	matchIndicator := ""
	if isMatch && !selected {
		matchIndicator = m.styles.match.Render("▸")
	}

	if selected {
		line1 := "▶ " + m.styles.agent.Render(agentLine) + sourceLabel
		lines = []string{line1}
		lines = append(lines, m.formatDescLines(item.Desc, width, maxDescLines, true)...)
	} else {
		line1 := iconSt.Render(icon+" ") + m.styles.agent.Render(agentLine) + sourceLabel + matchIndicator
		if count := m.unread[item.ID]; count > 0 {
			line1 += m.styles.warningBadge(fmt.Sprintf(" +%d", count))
		}
		lines = []string{line1}
		lines = append(lines, m.formatDescLines(item.Desc, width, maxDescLines, false)...)
	}

	if len(item.Skills) > 0 {
		skillLine := truncateLineCells("["+strings.Join(item.Skills, " · ")+"]", width-2)
		lines = append(lines, m.styles.skill.Render("  "+skillLine))
	}

	if item.Status == team.TaskError || item.Status == team.TaskBlocked || item.Status == team.TaskProtocolIncomplete {
		if failure := team.FailureDisplayText(item); failure != "" {
			errLine := truncateLineCells(strings.ReplaceAll(failure, "\n", " | "), width-4)
			lines = append(lines, m.styles.errorIcon.Render("  ✗ "+errLine))
		}
	}

	if selected {
		styledLines := make([]string, len(lines))
		for i, l := range lines {
			styledLines[i] = m.styles.selectedBG.Width(width).Render(m.styles.selectedFG.Render(l))
		}
		return styledLines
	}
	return lines
}

func (m Model) formatDescLines(desc string, width, maxLines int, selected bool) []string {
	wrapped := wrapCells(desc, width-2, maxLines)
	if len(wrapped.Lines) == 0 {
		return nil
	}
	result := make([]string, len(wrapped.Lines))
	for i, line := range wrapped.Lines {
		if selected {
			result[i] = "  " + line
		} else {
			result[i] = m.styles.dim.Render("  " + line)
		}
	}
	return result
}

func (m Model) sourceTag(source string) string {
	switch source {
	case team.TaskSourceAgent:
		return m.styles.dim.Render(" [A]")
	case team.TaskSourceSubagent:
		return m.styles.dim.Render(" [S]")
	default:
		return ""
	}
}

func (m *Model) scrollCursorIntoView() {
	off := &m.scrollOff[m.col]
	items := m.colItems(m.col)
	if len(items) == 0 {
		*off = 0
		return
	}
	if m.row >= len(items) {
		m.row = len(items) - 1
	}
	if m.row < 0 {
		m.row = 0
	}

	height := m.colBodyHeight()
	if height <= 2 {
		*off = m.row
		return
	}
	availableLines := height - 2
	if availableLines <= 0 {
		availableLines = 1
	}

	colW := 0
	if m.width >= 9 {
		colW = (m.width - 5) / 6
	}

	itemHeights := make([]int, len(items))
	for i, item := range items {
		h := len(m.itemLines(item, false, false, colW))
		if h == 0 {
			h = 2
		}
		itemHeights[i] = h
	}

	itemStartLine := make([]int, len(items))
	line := 0
	for i := 0; i < len(items); i++ {
		itemStartLine[i] = line
		line += itemHeights[i]
		if i < len(items)-1 {
			line++
		}
	}

	selectedStart := itemStartLine[m.row]
	selectedEnd := selectedStart + itemHeights[m.row]

	visibleStart := itemStartLine[*off]
	visibleEnd := visibleStart + availableLines

	if selectedStart < visibleStart {
		targetOff := m.row
		linesAbove := availableLines / 3
		cumLines := 0
		for targetOff > 0 && cumLines < linesAbove {
			targetOff--
			cumLines += itemHeights[targetOff]
			cumLines++
		}
		*off = targetOff
		return
	}

	if selectedEnd > visibleEnd || selectedStart >= visibleEnd {
		targetOff := m.row
		linesAbove := availableLines / 3
		cumLines := 0
		for targetOff > 0 && cumLines < linesAbove {
			targetOff--
			cumLines += itemHeights[targetOff]
			cumLines++
		}
		*off = targetOff
	}
}

func (m *Model) clampScroll() {
	for c := 0; c < 6; c++ {
		items := m.colItems(c)
		if len(items) == 0 {
			m.scrollOff[c] = 0
		} else if m.scrollOff[c] > len(items)-1 {
			m.scrollOff[c] = len(items) - 1
		}
	}
}

func (m Model) colBodyHeight() int {
	feedH := m.countFeedLines()
	feedTotal := 0
	if feedH > 0 {
		feedTotal = feedH + 1
	}
	progressH := 0
	if m.renderProgressBar(m.width) != "" {
		progressH = 1
	}
	h := m.height - m.promptWidgetHeight() - 1 - progressH - m.statusAreaHeight() - 1 - feedTotal - 2
	if h < 2 {
		return 2
	}
	return h
}

func (m Model) taskIconStyle(s team.TaskStatus) (string, lipgloss.Style) {
	switch s {
	case team.TaskInProgress:
		return "◑", m.styles.progressIcon
	case team.TaskVerifying:
		return "◔", m.styles.progressIcon
	case team.TaskDone:
		return "●", m.styles.doneIcon
	case team.TaskError:
		return "✗", m.styles.errorIcon
	case team.TaskBlocked:
		return "⚠", m.styles.errorIcon
	case team.TaskSkipped:
		return "—", m.styles.skippedIcon
	case team.TaskPlanned:
		return "◎", m.styles.dim
	case team.TaskPaused:
		return "◐", m.styles.pausedIcon
	}
	return "○", m.styles.pendingIcon
}
