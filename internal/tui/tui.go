package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
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

type AgentInfoEntry struct {
	Name string
	Role string
}

type TeamInfo struct {
	AvailableTeams []string
	TeamName       string
	Agents         []AgentInfoEntry
	MemoryEnabled  bool
	MemoryModel    string
	Skills         []string
	SidecarModel   string
	GuardModel     string
}

type TeamInfoMsg struct{ Info TeamInfo }

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
	matchStyle   = lipgloss.NewStyle().Background(lipgloss.Color("55")).Foreground(lipgloss.Color("15"))

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

	boldStyle    = lipgloss.NewStyle().Bold(true)
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	teamStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	infoBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("14")).
			Padding(1, 3)
)

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	prompt    string
	tasks     []*team.TodoItem
	logs      map[string][]string // todoID → rendered log lines
	coordItem *team.TodoItem

	col int // 0=pending 1=in_progress 2=done 3=skip 4=error
	row int // cursor within focused column

	scrollOff [5]int // scroll offset per column (index of first visible item)

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

	inPromptInput  bool
	promptInput    textinput.Model
	PromptInjectCh chan string

	inSearch      bool
	searchInput   textinput.Model
	searchQuery   string
	searchResults []*team.TodoItem
	searchIdx     int

	inInfo   bool
	teamInfo TeamInfo

	wrapUpRequested bool
	WrapUpCh        chan struct{}
}

// New creates a fresh model with the user's original prompt shown at the top.
func New(prompt string) Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "Type additional prompt..."
	ti.CharLimit = 500

	si := textinput.New()
	si.Prompt = "/"
	si.Placeholder = "Search tasks..."
	si.CharLimit = 200

	return Model{
		prompt:         prompt,
		logs:           make(map[string][]string),
		promptInput:    ti,
		searchInput:    si,
		PromptInjectCh: make(chan string, 16),
		WrapUpCh:       make(chan struct{}, 2),
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

	case TeamInfoMsg:
		m.teamInfo = msg.Info

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
		if m.wrapUpRequested {
			return m, tea.Quit
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
		if m.inInfo {
			return m.updateInfo(msg)
		}
		if m.inSearch {
			return m.updateSearch(msg)
		}
		if m.inPromptInput {
			return m.updatePromptInput(msg)
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

	// Forward non-key messages to the search input textinput when active.
	if m.inSearch {
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}

	// Forward non-key messages to the prompt input textinput when active.
	if m.inPromptInput {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
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
		return m.handleCtrlC()
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "i":
		m.inInfo = true
		return m, nil
	case "g":
		if m.vpReady {
			m.vp.GotoTop()
		}
		return m, nil
	case "G":
		if m.vpReady {
			m.vp.GotoBottom()
		}
		return m, nil
	case "n":
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx + 1) % len(m.searchResults)
			m.jumpToSearchMatch()
			m.inDetail = false
		}
		return m, nil
	case "N":
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx - 1 + len(m.searchResults)) % len(m.searchResults)
			m.jumpToSearchMatch()
			m.inDetail = false
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m Model) updateColumns(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	col := m.colItems(m.col)
	switch msg.String() {
	case "ctrl+c":
		return m.handleCtrlC()
	case "esc":
		if m.searchQuery != "" {
			m.searchQuery = ""
			m.searchResults = nil
			m.searchIdx = 0
			return m, nil
		}
		m.inConfirm = true
		m.confirmChoice = 0
		return m, nil
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "c":
		if !m.finished {
			m.inPromptInput = true
			m.promptInput.SetValue("")
			m.promptInput.Focus()
			return m, textinput.Blink
		}
	case "i":
		m.inInfo = true
		return m, nil
	case "/":
		m.inSearch = true
		m.searchInput.SetValue("")
		m.searchInput.Focus()
		return m, textinput.Blink
	case "n":
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx + 1) % len(m.searchResults)
			m.jumpToSearchMatch()
		}
	case "N":
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx - 1 + len(m.searchResults)) % len(m.searchResults)
			m.jumpToSearchMatch()
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
	case "g":
		if len(col) > 0 {
			m.row = 0
			m.scrollOff[m.col] = 0
		}
	case "G":
		if len(col) > 0 {
			m.row = len(col) - 1
			m.scrollCursorIntoView()
		}
	case "ctrl+d":
		halfPage := max(m.colBodyHeight()/2, 1)
		newRow := m.row + halfPage
		if newRow >= len(col) {
			newRow = len(col) - 1
		}
		if len(col) > 0 {
			m.row = newRow
			m.scrollCursorIntoView()
		}
	case "ctrl+u":
		halfPage := max(m.colBodyHeight()/2, 1)
		newRow := m.row - halfPage
		if newRow < 0 {
			newRow = 0
		}
		m.row = newRow
		m.scrollCursorIntoView()
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.row = 0
			m.scrollOff[m.col] = 0
		}
	case "tab":
		m.col = (m.col + 1) % 5
		m.row = 0
		m.scrollOff[m.col] = 0
	case "right", "l":
		if m.col < 4 {
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

func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.wrapUpRequested {
		return m, tea.Quit
	}
	m.wrapUpRequested = true
	select {
	case m.WrapUpCh <- struct{}{}:
	default:
	}
	m.statusText = dimStyle.Render("Wrapping up — press Ctrl+C again to force quit")
	return m, nil
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "h":
		if m.confirmChoice > 0 {
			m.confirmChoice--
		}
	case "right", "l", "tab":
		if m.confirmChoice < 1 {
			m.confirmChoice++
		}
	case "enter":
		if m.confirmChoice == 1 {
			m.inConfirm = false
			return m.handleCtrlC()
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
		m.inConfirm = false
		return m.handleCtrlC()
	}
	return m, nil
}

func (m Model) updatePromptInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		prompt := strings.TrimSpace(m.promptInput.Value())
		m.inPromptInput = false
		m.promptInput.SetValue("")
		m.promptInput.Blur()
		if prompt != "" {
			select {
			case m.PromptInjectCh <- prompt:
			default:
			}
		}
		return m, nil
	case "esc", "ctrl+c":
		m.inPromptInput = false
		m.promptInput.SetValue("")
		m.promptInput.Blur()
		return m, nil
	default:
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return m, cmd
	}
}

func (m Model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		query := strings.TrimSpace(m.searchInput.Value())
		m.inSearch = false
		m.searchInput.Blur()
		if query == "" {
			return m, nil
		}
		m.searchQuery = query
		m.searchResults = m.executeSearch(query)
		if len(m.searchResults) > 0 {
			m.searchIdx = 0
			m.jumpToSearchMatch()
		}
		return m, nil
	case "esc":
		m.inSearch = false
		m.searchInput.SetValue("")
		m.searchInput.Blur()
		return m, nil
	case "ctrl+c":
		m.inSearch = false
		m.searchInput.SetValue("")
		m.searchInput.Blur()
		return m.handleCtrlC()
	default:
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
}

func (m Model) updateInfo(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "i", "enter":
		m.inInfo = false
		return m, nil
	case "ctrl+c":
		m.inInfo = false
		return m.handleCtrlC()
	}
	return m, nil
}

func (m Model) executeSearch(query string) []*team.TodoItem {
	q := strings.ToLower(query)
	var results []*team.TodoItem
	for col := 0; col < 5; col++ {
		items := m.colItems(col)
		for _, item := range items {
			if strings.Contains(strings.ToLower(item.Agent), q) ||
				strings.Contains(strings.ToLower(item.Desc), q) ||
				strings.Contains(strings.ToLower(item.Detail), q) {
				results = append(results, item)
			}
		}
	}
	return results
}

func (m *Model) jumpToSearchMatch() {
	if len(m.searchResults) == 0 || m.searchIdx >= len(m.searchResults) {
		return
	}
	target := m.searchResults[m.searchIdx]
	for col := 0; col < 5; col++ {
		items := m.colItems(col)
		for i, item := range items {
			if item.ID == target.ID {
				m.col = col
				m.row = i
				m.scrollCursorIntoView()
				return
			}
		}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Initialising…"
	}
	if m.inAskUser {
		return m.askUserView()
	}
	if m.inInfo {
		return m.infoPanelView()
	}
	if m.inSearch {
		return m.searchView()
	}
	if m.inPromptInput {
		return m.promptInputView()
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

var colTitles = [5]string{"PENDING", "IN PROGRESS", "DONE", "SKIP", "ERROR"}

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

	// Four │ dividers, so each of five columns = (w-4)/5.
	colW := (w - 4) / 5

	c0 := m.renderCol(0, colW, bodyH)
	c1 := m.renderCol(1, colW, bodyH)
	c2 := m.renderCol(2, colW, bodyH)
	c3 := m.renderCol(3, colW, bodyH)
	c4 := m.renderCol(4, colW, bodyH)
	div := dimStyle.Render("│")

	body := lipgloss.JoinHorizontal(lipgloss.Top, c0, div, c1, div, c2, div, c3, div, c4)
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

func (m Model) itemLines(item *team.TodoItem, selected bool, isMatch bool, width int) []string {
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
	matchIndicator := ""
	if isMatch && !selected {
		matchIndicator = matchStyle.Render("▸")
	}

	if selected {
		line1 := "▶ " + agentStyle.Render(agentTrunc)
		line2 := "  " + descTrunc
		lines = []string{line1, line2}
	} else {
		line1 := iconSt.Render(icon+" ") + agentStyle.Render(agentTrunc) + matchIndicator
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
		itemLines := len(m.itemLines(items[i], false, false, 0))
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
	for c := 0; c < 5; c++ {
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
	if m.inInfo {
		return footerStyle.Render("i/esc close · ↑↓ scroll")
	}
	if m.inSearch {
		return footerStyle.Render("enter search · esc cancel")
	}
	if len(m.searchResults) > 0 {
		return footerStyle.Render(fmt.Sprintf("n/N next/prev match (%d/%d) · / search · i info · esc clear · g/G top/bot · ctrl+d/u half-page · ↑↓ j/k · ←→ h/l · enter detail · q quit", m.searchIdx+1, len(m.searchResults)))
	}
	if m.finished {
		return footerStyle.Render("g/G top/bot · ctrl+d/u half-page · / search · i info · ↑↓ j/k · ←→ h/l · enter detail · q quit")
	}
	return footerStyle.Render("g/G top/bot · ctrl+d/u half-page · / search · i info · c prompt · ↑↓ j/k · ←→ h/l · enter detail · esc quit")
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

var promptInputBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("12")).
	Padding(1, 2)

func (m Model) promptInputView() string {
	m.promptInput.Width = max(m.width-8, 20)
	content := promptStyle.Render("─── Additional Prompt ───") + "\n\n" +
		m.promptInput.View() + "\n\n" +
		dimStyle.Render("enter submit · esc cancel")
	dialog := promptInputBoxStyle.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

var searchBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("6")).
	Padding(1, 2)

func (m Model) searchView() string {
	m.searchInput.Width = max(m.width-8, 20)
	content := promptStyle.Render("─── Search ───") + "\n\n" +
		m.searchInput.View() + "\n\n" +
		dimStyle.Render("enter search · esc cancel")
	dialog := searchBoxStyle.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m Model) infoPanelView() string {
	info := m.teamInfo
	var b strings.Builder
	b.WriteString(headerStyle.Render("─── Info ───"))
	b.WriteString("\n\n")

	if len(info.AvailableTeams) > 0 {
		b.WriteString(boldStyle.Render("Teams: "))
		b.WriteString(strings.Join(info.AvailableTeams, ", "))
		b.WriteString("\n")
	}

	if info.TeamName != "" {
		b.WriteString(boldStyle.Render("Team:  "))
		b.WriteString(teamStyle.Render(info.TeamName))
		b.WriteString("\n")
	}

	if len(info.Agents) > 0 {
		b.WriteString(boldStyle.Render("Agents: "))
		var agentDisplay []string
		for _, a := range info.Agents {
			agentDisplay = append(agentDisplay, fmt.Sprintf("%s (%s)", agentStyle.Render(a.Name), dimStyle.Render(a.Role)))
		}
		b.WriteString(strings.Join(agentDisplay, ", "))
		b.WriteString("\n")
	}

	if info.MemoryEnabled {
		b.WriteString(doneStyle.Render("✓") + " " + boldStyle.Render("Memory: "))
		b.WriteString("enabled")
		if info.MemoryModel != "" {
			b.WriteString(fmt.Sprintf(" (model: %s)", info.MemoryModel))
		}
		b.WriteString("\n")
	}

	if len(info.Skills) > 0 {
		b.WriteString(boldStyle.Render("Skills: "))
		b.WriteString(strings.Join(info.Skills, ", "))
		b.WriteString("\n")
	}

	if info.SidecarModel != "" {
		b.WriteString(boldStyle.Render("Sidecar: "))
		b.WriteString(info.SidecarModel)
		b.WriteString("\n")
	}

	if info.GuardModel != "" {
		b.WriteString(boldStyle.Render("Guard:   "))
		b.WriteString(info.GuardModel)
		b.WriteString("\n")
	}

	b.WriteString("\n" + dimStyle.Render("esc close"))

	content := b.String()
	dialog := infoBoxStyle.Render(content)
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
		case col == 2 && t.Status == team.TaskDone:
			out = append(out, t)
		case col == 3 && t.Status == team.TaskSkipped:
			out = append(out, t)
		case col == 4 && t.Status == team.TaskError:
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
