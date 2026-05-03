package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/utils"
)

// ── Public messages (sent from coordinator goroutine via p.Send) ──────────────

// TasksUpdatedMsg is sent whenever the coordinator's TODO list changes.
type TasksUpdatedMsg struct{ Items []*team.TodoItem }

type TaskLogMsg struct {
	TodoID string
	Line   string
}

type CoordItemMsg struct{ Item *team.TodoItem }

type CoordStatusMsg struct{ Status team.TaskStatus }

type FinishedMsg struct{}

// StatusBarMsg updates the status line shown between the prompt and the columns.
type StatusBarMsg struct{ Text string }

// ResultMsg carries the final coordinator answer shown when work completes.
type ResultMsg struct{ Text string }

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	promptStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	promptBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("13")).
			Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	dimStyle    = lipgloss.NewStyle().Faint(true)
	agentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	skillStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("6"))
	footerStyle = lipgloss.NewStyle().Faint(true)
	selectedFg  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	selectedBg  = lipgloss.NewStyle().Background(lipgloss.Color("237"))

	pendingIcon  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	progressIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	doneIcon     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	skippedIcon  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))

	toolCallStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	toolResStyle  = lipgloss.NewStyle().Faint(true)
	stepHdrStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	textLogStyle  = lipgloss.NewStyle().Faint(true)

	confirmBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("9")).
			Padding(1, 3)
	confirmHighlightStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	confirmNormalStyle    = lipgloss.NewStyle().Faint(true)

	resultBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("2")).
			Padding(0, 1)
	resultLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
)

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	prompt    string
	tasks     []*team.TodoItem
	logs      map[string][]string // todoID → rendered log lines
	coordItem *team.TodoItem

	col int // 0=pending 1=in_progress 2=done
	row int // cursor within focused column

	scrollOff [3]int // scroll offset per column (index of first visible item)

	inDetail bool
	detailID string
	vp       viewport.Model
	vpReady  bool

	inConfirm     bool // showing quit confirmation dialog
	confirmChoice int  // 0=no 1=yes

	width      int
	height     int
	finished   bool
	statusText string // current status shown in the status bar
	result     string // final coordinator answer shown when finished

	inAskUser bool
	ask       askState
}

// New creates a fresh model with the user's original prompt shown at the top.
func New(prompt string) Model {
	return Model{
		prompt: prompt,
		logs:   make(map[string][]string),
	}
}

func (m Model) Init() tea.Cmd { return nil }

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case AskUserMsg:
		var cmd tea.Cmd
		m.ask, cmd = initAskUser(msg, m.width)
		m.inAskUser = true
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.inAskUser {
			m.ask.ti.Width = askTIWidth(msg.Width)
		}
		if m.vpReady {
			m.vp.Width = msg.Width
			m.vp.Height = m.vpHeight()
		} else {
			m.vp = viewport.New(msg.Width, m.vpHeight())
			m.vpReady = true
		}
		if m.inDetail {
			m.vp.SetContent(m.buildDetailContent())
		}
		m.clampScroll()

	case TasksUpdatedMsg:
		m.tasks = msg.Items
		if m.coordItem != nil {
			m.tasks = append(m.tasks, m.coordItem)
		}
		col := m.colItems(m.col)
		if len(col) > 0 && m.row >= len(col) {
			m.row = len(col) - 1
		}
		if m.inDetail {
			m.vp.SetContent(m.buildDetailContent())
		}
		m.clampScroll()

	case TaskLogMsg:
		m.logs[msg.TodoID] = append(m.logs[msg.TodoID], msg.Line)
		if m.inDetail && m.detailID == msg.TodoID && m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
			m.vp.GotoBottom()
		}

	case CoordItemMsg:
		m.coordItem = msg.Item
		m.tasks = append(m.tasks, msg.Item)

	case CoordStatusMsg:
		if m.coordItem != nil {
			m.coordItem.Status = msg.Status
			for i, t := range m.tasks {
				if t.ID == team.CoordTodoID {
					m.tasks[i].Status = msg.Status
					break
				}
			}
		}

	case StatusBarMsg:
		m.statusText = msg.Text

	case ResultMsg:
		m.result = msg.Text

	case FinishedMsg:
		m.finished = true
		m.statusText = doneIcon.Render("✓") + dimStyle.Render("  All tasks completed")
		// The coordinator already called finalizeNormalCompletion() which marks
		// TaskPending → TaskSkipped and TaskInProgress → TaskDone via a
		// todos_updated event. This is a safety net for any stragglers.
		for i, t := range m.tasks {
			switch t.Status {
			case team.TaskInProgress:
				m.tasks[i].Status = team.TaskDone
			case team.TaskPending:
				m.tasks[i].Status = team.TaskSkipped
			}
		}
		if m.coordItem != nil {
			switch m.coordItem.Status {
			case team.TaskInProgress:
				m.coordItem.Status = team.TaskDone
			case team.TaskPending:
				m.coordItem.Status = team.TaskSkipped
			}
			for i, t := range m.tasks {
				if t.ID == team.CoordTodoID {
					m.tasks[i].Status = m.coordItem.Status
					break
				}
			}
		}

	case tea.MouseMsg:
		if m.inDetail {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		switch msg.Type {
		case tea.MouseWheelUp:
			if m.row > 0 {
				m.row--
				m.scrollCursorIntoView()
			}
		case tea.MouseWheelDown:
			col := m.colItems(m.col)
			if m.row < len(col)-1 {
				m.row++
				m.scrollCursorIntoView()
			}
		}

	case tea.KeyMsg:
		if m.inAskUser {
			return m.updateAskUser(msg)
		}
		if m.inConfirm {
			return m.updateConfirm(msg)
		}
		if m.inDetail {
			return m.updateDetail(msg)
		}
		return m.updateColumns(msg)
	}

	// Forward non-key messages to the textinput (cursor blink, paste, etc.)
	// when the ask_user free-text dialog is active.
	if m.inAskUser && m.ask.isFreeText() {
		var cmd tea.Cmd
		m.ask.ti, cmd = m.ask.ti.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.inDetail = false
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m Model) updateColumns(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	col := m.colItems(m.col)
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.inConfirm = true
		m.confirmChoice = 0
		return m, nil
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "up", "k":
		if m.row > 0 {
			m.row--
			m.scrollCursorIntoView()
		}
	case "down", "j":
		if m.row < len(col)-1 {
			m.row++
			m.scrollCursorIntoView()
		}
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.row = 0
			m.scrollOff[m.col] = 0
		}
	case "right", "l", "tab":
		if m.col < 2 {
			m.col++
			m.row = 0
			m.scrollOff[m.col] = 0
		}
	case "enter":
		if m.row < len(col) {
			m.detailID = col[m.row].ID
			m.inDetail = true
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
				m.vp.GotoBottom()
			}
		}
	}
	return m, nil
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "h":
		if m.confirmChoice > 0 {
			m.confirmChoice--
		}
	case "right", "l":
		if m.confirmChoice < 1 {
			m.confirmChoice++
		}
	case "enter":
		if m.confirmChoice == 1 {
			return m, tea.Quit
		}
		m.inConfirm = false
		return m, nil
	case "esc":
		m.inConfirm = false
		return m, nil
	case "n":
		m.inConfirm = false
		return m, nil
	case "y":
		return m, tea.Quit
	}
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Initialising…"
	}
	if m.inAskUser {
		return m.askUserView()
	}
	if m.inConfirm {
		return m.confirmView()
	}
	if m.inDetail {
		return m.detailView()
	}
	return m.columnsView()
}

// ── Column view ───────────────────────────────────────────────────────────────

var colTitles = [3]string{"PENDING", "IN PROGRESS", "DONE"}

func (m Model) renderPromptWidget(w int) string {
	// border(1) + padding(1) on each side = 4 overhead
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
	content := promptStyle.Render(labelPlain) + utils.TruncatePreview(m.prompt, textW)
	return promptBoxStyle.Width(innerW).Render(content)
}

const maxResultLines = 8

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
			lines = append(lines, dimStyle.Render(fmt.Sprintf("... (%d more lines)", remaining)))
		}
		labelPlain := "Result  "
		var sb strings.Builder
		sb.WriteString(resultLabelStyle.Render(labelPlain))
		for i, l := range lines {
			if i == 0 {
				sb.WriteString(utils.TruncatePreview(l, innerW-len([]rune(labelPlain))))
			} else {
				sb.WriteString("\n")
				sb.WriteString(utils.TruncatePreview(l, innerW))
			}
		}
		return resultBoxStyle.Width(innerW).Render(sb.String())
	}

	text := m.statusText
	if text == "" {
		if m.finished {
			return dimStyle.Render("  ✓ Done")
		}
		return dimStyle.Render("  ⟳ Initialising…")
	}
	// Text already carries its own lipgloss styling from the reporter.
	return "  " + text
}

func (m Model) columnsView() string {
	w := m.width
	if w < 9 {
		return ""
	}

	widget := m.renderPromptWidget(w)
	statusArea := m.renderStatusArea(w)
	statusH := m.statusAreaHeight()

	// widget(3) + blank(1) + statusArea(statusH) + blank(1) + blank(1) + footer(1)
	bodyH := m.height - 3 - 1 - statusH - 1 - 1 - 1
	if bodyH < 2 {
		bodyH = 2
	}

	// Two │ dividers, so each of three columns = (w-2)/3.
	colW := (w - 2) / 3

	c0 := m.renderCol(0, colW, bodyH)
	c1 := m.renderCol(1, colW, bodyH)
	c2 := m.renderCol(2, colW, bodyH)
	div := dimStyle.Render("│")

	body := lipgloss.JoinHorizontal(lipgloss.Top, c0, div, c1, div, c2)
	return widget + "\n" + statusArea + "\n" + body + "\n\n" + m.footer()
}

func (m Model) renderCol(col, width, height int) string {
	focused := col == m.col
	items := m.colItems(col)

	titleLabel := colTitles[col]
	count := fmt.Sprintf("(%d)", len(items))
	var titleLine string
	if focused {
		titleLine = headerStyle.Render(titleLabel) + " " + dimStyle.Render(count)
	} else {
		titleLine = dimStyle.Render(titleLabel + " " + count)
	}

	var sb strings.Builder
	sb.WriteString(utils.TruncatePreview(titleLine, width) + "\n")
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
		for _, l := range m.itemLines(items[i], selected, width) {
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

func (m Model) itemLines(item *team.TodoItem, selected bool, width int) []string {
	icon, iconSt := taskIconStyle(item.Status)
	agentTrunc := utils.TruncatePreview(item.Agent, width-3)
	descTrunc := utils.TruncatePreview(item.Desc, width-2)

	if item.ID == team.CoordTodoID {
		coordLabel := dimStyle.Render("(coordinator)")
		agentTrunc = utils.TruncatePreview(item.Agent, width-len("(coordinator)")-4)
		descTrunc = utils.TruncatePreview("coordinating", width-2)

		var lines []string
		if selected {
			line1 := "▶ " + agentStyle.Render(agentTrunc) + " " + coordLabel
			line2 := "  " + descTrunc
			lines = []string{line1, line2}
		} else {
			line1 := iconSt.Render(icon+" ") + agentStyle.Render(agentTrunc) + " " + coordLabel
			line2 := dimStyle.Render("  " + descTrunc)
			lines = []string{line1, line2}
		}
		if selected {
			styledLines := make([]string, len(lines))
			for i, l := range lines {
				styledLines[i] = selectedBg.Width(width).Render(selectedFg.Render(l))
			}
			return styledLines
		}
		return lines
	}

	var lines []string

	if selected {
		line1 := "▶ " + agentStyle.Render(agentTrunc)
		line2 := "  " + descTrunc
		lines = []string{line1, line2}
	} else {
		line1 := iconSt.Render(icon+" ") + agentStyle.Render(agentTrunc)
		line2 := dimStyle.Render("  " + descTrunc)
		lines = []string{line1, line2}
	}

	if len(item.Skills) > 0 {
		skillTrunc := utils.TruncatePreview("["+strings.Join(item.Skills, " · ")+"]", width-2)
		lines = append(lines, skillStyle.Render("  "+skillTrunc))
	}

	if item.Status == team.TaskError && item.Detail != "" {
		errTrunc := utils.TruncatePreview("✗ "+item.Detail, width-2)
		lines = append(lines, errorIcon.Render("  "+errTrunc))
	}

	if selected {
		styledLines := make([]string, len(lines))
		for i, l := range lines {
			styledLines[i] = selectedBg.Width(width).Render(selectedFg.Render(l))
		}
		return styledLines
	}
	return lines
}

func (m *Model) scrollCursorIntoView() {
	off := &m.scrollOff[m.col]
	if m.row < *off {
		*off = m.row
		return
	}
	items := m.colItems(m.col)
	height := m.colBodyHeight()
	lineCount := 2
	for i := *off; i < len(items); i++ {
		itemLines := len(m.itemLines(items[i], false, 0))
		if itemLines == 0 {
			itemLines = 2
		}
		prevLineCount := lineCount
		lineCount += itemLines
		if i < len(items)-1 {
			lineCount++
		}
		if i == m.row && prevLineCount >= height {
			*off++
		}
		if i >= m.row {
			break
		}
	}
}

func (m *Model) clampScroll() {
	for c := 0; c < 3; c++ {
		items := m.colItems(c)
		if len(items) == 0 {
			m.scrollOff[c] = 0
		} else if m.scrollOff[c] > len(items)-1 {
			m.scrollOff[c] = len(items) - 1
		}
	}
}

func (m Model) colBodyHeight() int {
	// widget(3) + blank(1) + statusArea + blank(1) + blank(1) + footer(1)
	return m.height - 7 - m.statusAreaHeight()
}

func taskIconStyle(s team.TaskStatus) (string, lipgloss.Style) {
	switch s {
	case team.TaskInProgress:
		return "◑", progressIcon
	case team.TaskDone:
		return "●", doneIcon
	case team.TaskError:
		return "✗", errorIcon
	case team.TaskSkipped:
		return "—", skippedIcon
	}
	return "○", pendingIcon
}

func (m Model) footer() string {
	if m.finished {
		return footerStyle.Render("↑↓ navigate  ←→ columns  ↕ scroll  enter detail  q quit")
	}
	return footerStyle.Render("↑↓ navigate  ←→ columns  ↕ scroll  enter detail  esc quit")
}

func (m Model) confirmView() string {
	noLabel := " No "
	yesLabel := " Yes "
	noStyled := confirmNormalStyle.Render(noLabel)
	yesStyled := confirmNormalStyle.Render(yesLabel)
	if m.confirmChoice == 0 {
		noStyled = confirmHighlightStyle.Render(noLabel)
	} else {
		yesStyled = confirmHighlightStyle.Render(yesLabel)
	}
	buttons := noStyled + "  " + yesStyled
	dialog := confirmBoxStyle.Render("  Quit hufu?" + "\n\n" + buttons + "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

// ── Detail view ───────────────────────────────────────────────────────────────

func (m Model) detailView() string {
	item := m.findTask(m.detailID)
	if item == nil {
		return "task not found — press esc"
	}

	back := dimStyle.Render("← esc")
	agentLabel := agentStyle.Render(item.Agent)
	if item.ID == team.CoordTodoID {
		agentLabel += " " + dimStyle.Render("(coordinator)")
	}
	heading := fmt.Sprintf("%s  %s / %s",
		back,
		agentLabel,
		utils.TruncatePreview(item.Desc, m.width-30))

	status := string(item.Status)
	meta := dimStyle.Render("status: " + status)
	if item.Model != "" {
		meta += "  " + dimStyle.Render("model: "+item.Model)
	}
	if len(item.Skills) > 0 {
		meta += "  " + skillStyle.Render("skills: "+strings.Join(item.Skills, ", "))
	}

	sep := dimStyle.Render(strings.Repeat("─", m.width))

	header := heading + "\n" + meta + "\n" + sep
	if !m.vpReady {
		return header
	}
	return header + "\n" + m.vp.View()
}

// vpHeight is the number of lines available for the detail viewport.
func (m Model) vpHeight() int {
	h := m.height - 3 // 3 header lines
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) buildDetailContent() string {
	lines := m.logs[m.detailID]
	if len(lines) == 0 {
		item := m.findTask(m.detailID)
		if item != nil && item.Status == team.TaskSkipped {
			return dimStyle.Render("  (task was not executed)")
		}
		return dimStyle.Render("  (no output yet)")
	}
	return strings.Join(lines, "\n")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) colItems(col int) []*team.TodoItem {
	var out []*team.TodoItem
	for _, t := range m.tasks {
		switch {
		case col == 0 && t.Status == team.TaskPending:
			out = append(out, t)
		case col == 1 && t.Status == team.TaskInProgress:
			out = append(out, t)
		case col == 2 && (t.Status == team.TaskDone || t.Status == team.TaskError || t.Status == team.TaskSkipped):
			out = append(out, t)
		}
	}
	return out
}

func (m Model) findTask(id string) *team.TodoItem {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// ── Log-line renderers (called by the status reporter in cmd/hufu) ────────────

// RenderStep returns a formatted step-header log line.
func RenderStep(stepNumber int) string {
	return stepHdrStyle.Render(fmt.Sprintf("[step %d]", stepNumber))
}

// RenderToolCall returns a formatted tool-call log line.
func RenderToolCall(toolName, args string) string {
	if len(args) > 300 {
		args = args[:300] + "…"
	}
	return toolCallStyle.Render("⟹ "+toolName) + "\n" + dimStyle.Render("  "+args)
}

// RenderToolResult returns a formatted tool-result log line.
func RenderToolResult(toolName, result string) string {
	if len(result) > 300 {
		result = result[:300] + "…"
	}
	return toolResStyle.Render("✓ "+toolName) + "\n" + dimStyle.Render("  "+result)
}

// RenderText returns a formatted text-delta log line.
func RenderText(text string) string {
	return textLogStyle.Render("💬 " + strings.TrimSpace(text))
}
