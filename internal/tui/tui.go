package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anomalyco/hufu/internal/team"
)

// ── Public messages (sent from coordinator goroutine via p.Send) ──────────────

// TasksUpdatedMsg is sent whenever the coordinator's TODO list changes.
type TasksUpdatedMsg struct{ Items []*team.TodoItem }

// TaskLogMsg appends one rendered log line to a task's detail view.
type TaskLogMsg struct {
	TodoID string
	Line   string
}

// FinishedMsg signals that all coordinator work is done.
type FinishedMsg struct{}

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

	toolCallStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	toolResStyle  = lipgloss.NewStyle().Faint(true)
	stepHdrStyle  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	textLogStyle  = lipgloss.NewStyle().Faint(true)
)

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	prompt string
	tasks  []*team.TodoItem
	logs   map[string][]string // todoID → rendered log lines

	col int // 0=pending 1=in_progress 2=done
	row int // cursor within focused column

	inDetail bool
	detailID string
	vp       viewport.Model
	vpReady  bool

	width    int
	height   int
	finished bool
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
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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

	case TasksUpdatedMsg:
		m.tasks = msg.Items
		col := m.colItems(m.col)
		if len(col) > 0 && m.row >= len(col) {
			m.row = len(col) - 1
		}
		if m.inDetail {
			m.vp.SetContent(m.buildDetailContent())
		}

	case TaskLogMsg:
		m.logs[msg.TodoID] = append(m.logs[msg.TodoID], msg.Line)
		if m.inDetail && m.detailID == msg.TodoID && m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
			m.vp.GotoBottom()
		}

	case FinishedMsg:
		m.finished = true

	case tea.KeyMsg:
		if m.inDetail {
			return m.updateDetail(msg)
		}
		return m.updateColumns(msg)
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
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "up", "k":
		if m.row > 0 {
			m.row--
		}
	case "down", "j":
		if m.row < len(col)-1 {
			m.row++
		}
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.row = 0
		}
	case "right", "l", "tab":
		if m.col < 2 {
			m.col++
			m.row = 0
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

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Initialising…"
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
	content := promptStyle.Render(labelPlain) + truncRaw(m.prompt, textW)
	return promptBoxStyle.Width(innerW).Render(content)
}

func (m Model) columnsView() string {
	w := m.width
	if w < 9 {
		return ""
	}

	widget := m.renderPromptWidget(w)
	// widget = 3 lines (top-border + content + bottom-border)
	bodyH := m.height - 3 - 1 - 1 - 1 // widget(3) + blank(1) + blank(1) + footer(1)
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
	return widget + "\n" + body + "\n\n" + m.footer()
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
	sb.WriteString(truncRaw(titleLine, width) + "\n")
	sb.WriteString("\n")
	usedLines := 2

	for i, item := range items {
		if usedLines >= height {
			break
		}
		selected := focused && i == m.row
		for _, l := range m.itemLines(item, selected, width) {
			if usedLines >= height {
				break
			}
			sb.WriteString(l + "\n")
			usedLines++
		}
		// blank separator between items
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
	agentTrunc := truncRaw(item.Agent, width-3)
	descTrunc := truncRaw(item.Desc, width-2)

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
		skillTrunc := truncRaw("["+strings.Join(item.Skills, " · ")+"]", width-2)
		lines = append(lines, skillStyle.Render("  "+skillTrunc))
	}

	if item.Status == team.TaskError && item.Detail != "" {
		errTrunc := truncRaw("✗ "+item.Detail, width-2)
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

func taskIconStyle(s team.TaskStatus) (string, lipgloss.Style) {
	switch s {
	case team.TaskInProgress:
		return "◑", progressIcon
	case team.TaskDone:
		return "●", doneIcon
	case team.TaskError:
		return "✗", errorIcon
	}
	return "○", pendingIcon
}

func (m Model) footer() string {
	if m.finished {
		return footerStyle.Render("↑↓ navigate  ←→ columns  enter detail  q quit")
	}
	return footerStyle.Render("↑↓ navigate  ←→ columns  enter detail  ctrl+c cancel")
}

// ── Detail view ───────────────────────────────────────────────────────────────

func (m Model) detailView() string {
	item := m.findTask(m.detailID)
	if item == nil {
		return "task not found — press esc"
	}

	back := dimStyle.Render("← esc")
	heading := fmt.Sprintf("%s  %s / %s",
		back,
		agentStyle.Render(item.Agent),
		truncRaw(item.Desc, m.width-30))

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
		case col == 2 && (t.Status == team.TaskDone || t.Status == team.TaskError):
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

// truncRaw truncates s to at most maxW runes (ANSI-unaware; use only on plain
// strings before applying lipgloss styles).
func truncRaw(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxW {
		return s
	}
	if maxW <= 1 {
		return "…"
	}
	return string(runes[:maxW-1]) + "…"
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
