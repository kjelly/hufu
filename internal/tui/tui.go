package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/utils"
)

// ── Public messages (sent from coordinator goroutine via p.Send) ──────────────

// TasksUpdatedMsg is sent whenever the coordinator's TODO list changes.
type TasksUpdatedMsg struct{ Items []*team.TodoItem }

type TaskLogMsg struct {
	TodoID string
	Line   string
	Model  string
}

type CoordItemMsg struct{ Item *team.TodoItem }

type CoordStatusMsg struct{ Status team.TaskStatus }

type FinishedMsg struct{}

// StatusBarMsg updates the status line shown between the prompt and the columns.
type StatusBarMsg struct{ Text string }

// ResultMsg carries the final coordinator answer shown when work completes.
type ResultMsg struct{ Text string }

type AgentInfoEntry struct {
	Name  string
	Role  string
	Model string
}

type TeamInfo struct {
	AvailableTeams []string
	TeamName       string
	DefaultModel   string
	Agents         []AgentInfoEntry
	MemoryEnabled  bool
	MemoryModel    string
	Skills         []string
	SidecarModel   string
	GuardModel     string
	Workspace      string
	TeamDir        string
	SSHSessions    int
	IsChat         bool
}

type TeamInfoMsg struct{ Info TeamInfo }

type SSHSessionsMsg struct{ Count int }

type WrapUpMsg struct{}

type AskUserCancelMsg struct{}

type detailRefreshMsg struct{}

type copySuccessMsg struct{ Lines int }

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
	pausedIcon   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	doneIcon     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errorIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	skippedIcon  = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	matchStyle   = lipgloss.NewStyle().Background(lipgloss.Color("55")).Foreground(lipgloss.Color("15"))
	wrapUpStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	visualStyle  = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	visualLabel  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))

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

	col int // 0=pending 1=planned 2=in_progress 3=done 4=skip 5=error
	row int // cursor within focused column

	scrollOff [6]int // scroll offset per column (index of first visible item)

	inDetail bool
	detailID string
	vp       viewport.Model
	vpReady  bool
	inResult bool

	inMemory    bool
	memoryVP    viewport.Model
	memoryReady bool

	inConfirm     bool // showing quit confirmation dialog
	confirmChoice int  // 0=no 1=yes 2=force

	width      int
	height     int
	finished   bool
	IsChat     bool
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

	inHelp bool

	wrapUpRequested bool
	WrapUpCh        chan struct{}
	ReportCh        chan struct{}

	mouseEnabled         bool // mouse tracking is currently on
	mouseManuallyEnabled bool // user explicitly toggled mouse on with 'm'

	inActivityLog          bool     // 全螢幕 activity log 模式
	recentLogs             []string // last N activity log entries (circular buffer, max 500 entries; each may be multi-line, capped at maxFeedLines rendered lines)
	detailRefreshScheduled bool

	inVisual    bool // VISUAL mode active in detail view
	cursorLine  int  // current line index within detail logs
	visualStart int  // selection start line index
	visualEnd   int  // selection end line index
}

func enableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.EnableMouseCellMotion() }
}

func disableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.DisableMouse() }
}

// New creates a fresh model with the user's original prompt shown at the top.
func New(prompt string, teamInfo TeamInfo) Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "Type additional prompt..."
	ti.CharLimit = 500

	si := textinput.New()
	si.Prompt = "/"
	si.Placeholder = "Search tasks..."
	si.CharLimit = 200

	m := Model{
		prompt:         prompt,
		logs:           make(map[string][]string),
		promptInput:    ti,
		searchInput:    si,
		PromptInjectCh: make(chan string, 16),
		WrapUpCh:       make(chan struct{}, 2),
		ReportCh:       make(chan struct{}, 1),
		teamInfo:       teamInfo,
		IsChat:         teamInfo.IsChat,
	}

	if m.IsChat && prompt == "" {
		m.inPromptInput = true
		m.promptInput.Focus()
	}

	return m
}

func (m Model) Init() tea.Cmd {
	if m.IsChat && m.inPromptInput {
		return textinput.Blink
	}
	return nil
}

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
			if m.inActivityLog {
				m.vp.Height = msg.Height - 4
			} else {
				m.vp.Height = m.vpHeight()
			}
		} else {
			h := m.vpHeight()
			if m.inActivityLog {
				h = msg.Height - 4
			}
			m.vp = viewport.New(msg.Width, h)
			m.vpReady = true
		}
		if m.inDetail {
			m.vp.SetContent(m.buildDetailContent())
		}
		if m.inResult {
			m.vp.SetContent(m.result)
		}
		if m.inActivityLog {
			m.vp.SetContent(m.formatActivityLogContent())
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
		m.scrollCursorIntoView()

	case TaskLogMsg:
		line := msg.Line
		if msg.Model != "" {
			line = dimStyle.Render("["+msg.Model+"] ") + line
		}
		m.logs[msg.TodoID] = append(m.logs[msg.TodoID], line)
		m.trimTaskLogs(msg.TodoID)
		if m.inDetail && m.detailID == msg.TodoID && m.vpReady && !m.detailRefreshScheduled {
			m.detailRefreshScheduled = true
			return m, func() tea.Msg {
				time.Sleep(80 * time.Millisecond)
				return detailRefreshMsg{}
			}
		}
		m.recentLogs = append(m.recentLogs, line)
		if len(m.recentLogs) > 500 {
			m.recentLogs = m.recentLogs[len(m.recentLogs)-500:]
		}

	case detailRefreshMsg:
		m.detailRefreshScheduled = false
		if m.inDetail && m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
			contentLines := len(m.logs[m.detailID])
			if contentLines > 0 {
				m.cursorLine = contentLines - 1
				m.followCursor()
			}
		}
		return m, nil
	case copySuccessMsg:
		m.statusText = doneStyle.Render(fmt.Sprintf("✓ Copied %d lines to clipboard", msg.Lines))

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

	case SSHSessionsMsg:
		m.teamInfo.SSHSessions = msg.Count

	case FinishedMsg:
		m.finished = true
		m.recentLogs = nil
		m.statusText = doneIcon.Render("✓") + dimStyle.Render("  All tasks completed")
		// The coordinator already called finalizeNormalCompletion() which marks
		// TaskPending → TaskSkipped and TaskInProgress → TaskDone via a
		// todos_updated event. This is a safety net for any stragglers.
		for i, t := range m.tasks {
			switch t.Status {
			case team.TaskInProgress, team.TaskPaused, team.TaskVerifying:
				m.tasks[i].Status = team.TaskDone
			case team.TaskPending, team.TaskPlanned:
				m.tasks[i].Status = team.TaskSkipped
			}
		}
		if m.coordItem != nil {
			switch m.coordItem.Status {
			case team.TaskInProgress, team.TaskVerifying:
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
		if m.wrapUpRequested && !m.inAskUser {
			return m, tea.Quit
		}

	case WrapUpMsg:
		return m.handleCtrlC()

	case AskUserCancelMsg:
		if m.inAskUser && m.ask.req != nil {
			select {
			case m.ask.req.ReplyCh <- marshalAskResp(nil, ""):
			default:
			}
			m.inAskUser = false
		}
		if m.finished && m.wrapUpRequested {
			return m, tea.Quit
		}

	case tea.MouseMsg:
		if m.inDetail {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)

			item := m.findTask(m.detailID)
			if item != nil {
				header := m.renderDetailHeader(item)
				headerLines := len(strings.Split(header, "\n"))
				clickY := msg.Y - headerLines

				if clickY >= 0 && clickY < m.vp.Height {
					lines, ok := m.logs[m.detailID]
					if ok && len(lines) > 0 {
						width := m.vp.Width
						if width < 20 {
							width = 20
						}
						logIndex := m.mapRenderedLineToLogIndex(m.vp.YOffset+clickY, width)
						if logIndex >= 0 && logIndex < len(lines) {
							if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
								m.inVisual = true
								m.cursorLine = logIndex
								m.visualStart = logIndex
								m.visualEnd = logIndex
								m.vp.SetContent(m.buildDetailContent())
							} else if msg.Action == tea.MouseActionMotion && m.inVisual && msg.Button == tea.MouseButtonLeft {
								m.cursorLine = logIndex
								m.visualEnd = logIndex
								m.vp.SetContent(m.buildDetailContent())
							}
						}
					}
				}
			}

			if msg.Action == tea.MouseActionRelease && m.inVisual && msg.Button == tea.MouseButtonLeft {
				if m.visualStart != m.visualEnd {
					text := m.getVisualSelection()
					m.inVisual = false
					m.visualStart = 0
					m.visualEnd = 0
					if m.vpReady {
						m.vp.SetContent(m.buildDetailContent())
					}
					if text != "" {
						return m, copyToClipboard(text)
					}
				}
			}

			return m, cmd
		}
		if !m.mouseEnabled {
			return m, nil
		}
		if m.isCompact() {
			// Compact columns merge task states, so six-column hit testing is invalid.
			return m, nil
		}
		// Click on a task to select it or enter detail view
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			statusH := m.statusAreaHeight()
			promptH := m.promptWidgetHeight()
			feedH := m.countFeedLines()
			feedTotal := 0
			if feedH > 0 {
				feedTotal = feedH + 1
			}
			bodyH := m.colBodyHeight()
			colW := 0
			if m.width >= 9 {
				colW = (m.width - 5) / 6
			}

			// Header takes 2 lines (title + blank)
			// Subtract: widget + blank + status + blank + [feed + blank if present]
			clickY := msg.Y - promptH - 1 - statusH - 1 - feedTotal
			clickX := msg.X
			if clickY < 2 || clickY >= bodyH+2 {
				return m, nil
			}
			clickY -= 2 // skip header

			// Determine which column was clicked (6 columns, 5 dividers)
			clickedCol := -1
			xOffset := 0
			for c := 0; c < 6; c++ {
				colEnd := xOffset + colW
				if c == 5 {
					colEnd = m.width // last column takes remaining width
				}
				if clickX >= xOffset && clickX < colEnd {
					clickedCol = c
					break
				}
				xOffset = colEnd + 1 // +1 for divider
			}
			if clickedCol < 0 || clickedCol != m.col {
				return m, nil
			}

			// Determine which item was clicked
			items := m.colItems(clickedCol)
			start := m.scrollOff[clickedCol]
			lineCount := 2
			for i := start; i < len(items); i++ {
				itemLines := len(m.itemLines(items[i], false, false, colW))
				if itemLines == 0 {
					itemLines = 2
				}
				if clickY >= lineCount && clickY < lineCount+itemLines+1 {
					// Clicked on item i — enter detail view
					m.detailID = items[i].ID
					m.inDetail = true
					if m.vpReady {
						m.vp.SetContent(m.buildDetailContent())
						m.vp.GotoTop()
					}
					if !m.mouseEnabled {
						m.mouseEnabled = true
						return m, enableMouseCmd()
					}
					return m, nil
				}
				lineCount += itemLines
				if i < len(items)-1 {
					lineCount++ // blank line between items
				}
			}
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.row > 0 {
				m.row--
				m.scrollCursorIntoView()
			}
		case tea.MouseButtonWheelDown:
			col := m.colItems(m.col)
			if m.row < len(col)-1 {
				m.row++
				m.scrollCursorIntoView()
			}
		}

	case tea.KeyMsg:
		// Overlay dispatch: same priority order as View() — see overlay.go.
		switch m.currentOverlay() {
		case OverlayAskUser:
			return m.updateAskUser(msg)
		case OverlayHelp:
			return m.updateHelp(msg)
		case OverlayInfo:
			return m.updateInfo(msg)
		case OverlaySearch:
			return m.updateSearch(msg)
		case OverlayPromptInput:
			return m.updatePromptInput(msg)
		case OverlayConfirm:
			return m.updateConfirm(msg)
		case OverlayDetail:
			return m.updateDetail(msg)
		case OverlayResult:
			return m.updateResult(msg)
		case OverlayMemory:
			return m.updateMemory(msg)
		case OverlayActivityLog:
			return m.updateActivityLog(msg)
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
	if m.inVisual {
		return m.updateVisual(msg)
	}
	switch msg.String() {
	case "esc", "backspace":
		m.inDetail = false
		m.inVisual = false
		m.visualStart = 0
		m.visualEnd = 0
		if !m.mouseManuallyEnabled {
			m.mouseEnabled = false
			return m, disableMouseCmd()
		}
		return m, nil
	case "ctrl+c":
		return m.handleCtrlC()
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "r":
		return m.handleReportKey()
	case "i":
		m.inInfo = true
		return m, nil
	case "v":
		return m.enterVisual()
	case "j", "down":
		contentLines := len(m.logs[m.detailID])
		if contentLines > 0 && m.cursorLine < contentLines-1 {
			m.cursorLine++
			m.followCursor()
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		return m, nil
	case "k", "up":
		if m.cursorLine > 0 {
			m.cursorLine--
			m.followCursor()
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		return m, nil
	case "ctrl+d", "J":
		h := m.vp.Height
		if h <= 0 {
			h = 10
		}
		quarterPage := h / 4
		if quarterPage < 1 {
			quarterPage = 1
		}
		m.vp.SetYOffset(m.vp.YOffset + quarterPage)
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	case "ctrl+u", "K":
		h := m.vp.Height
		if h <= 0 {
			h = 10
		}
		quarterPage := h / 4
		if quarterPage < 1 {
			quarterPage = 1
		}
		m.vp.SetYOffset(m.vp.YOffset - quarterPage)
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	case "g":
		m.cursorLine = 0
		m.followCursor()
		if m.vpReady {
			m.vp.GotoTop()
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	case "G":
		contentLines := len(m.logs[m.detailID])
		if contentLines > 0 {
			m.cursorLine = contentLines - 1
			m.followCursor()
			if m.vpReady {
				m.vp.GotoBottom()
				m.vp.SetContent(m.buildDetailContent())
			}
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
	case "M":
		m.inDetail = false
		m.inMemory = true
		m.loadMemoryContent()
		return m, nil
	case "m":
		m.mouseManuallyEnabled = !m.mouseManuallyEnabled
		m.mouseEnabled = m.mouseManuallyEnabled
		if m.mouseEnabled {
			return m, enableMouseCmd()
		}
		return m, disableMouseCmd()
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// followCursor updates the viewport YOffset to keep the cursor visible.
// Thread Safety: This method directly modifies m.vp.YOffset, which is safe
// because Bubble Tea's Update loop is single-threaded. DO NOT call this from
// goroutines or outside the Update loop.
func (m *Model) followCursor() {
	if !m.vpReady {
		return
	}
	h := m.vp.Height
	if h <= 0 {
		return
	}

	lines, ok := m.logs[m.detailID]
	if !ok || len(lines) == 0 {
		return
	}
	width := m.width
	if width < 20 {
		width = 20
	}
	wrapW := width - 2

	currentY := 0
	cursorYStart := -1
	cursorYEnd := -1
	var cursorLineHeight int

	for i, entry := range lines {
		// Only wrap until we find the cursor to save time (O(n) but fast n)
		lineCount := 1
		if strings.Contains(entry, "\n") || utf8.RuneCountInString(entry) > wrapW {
			wrapped := wrapText(entry, wrapW)
			lineCount = strings.Count(wrapped, "\n") + 1
		}

		if i == m.cursorLine {
			cursorYStart = currentY
			cursorYEnd = currentY + lineCount - 1
			cursorLineHeight = lineCount
			break
		}
		currentY += lineCount
	}

	if cursorYStart == -1 {
		return
	}

	// Adjust YOffset
	if cursorYStart < m.vp.YOffset {
		// Moving up: ensure start of entry is visible
		m.vp.YOffset = cursorYStart
	} else if cursorYEnd >= m.vp.YOffset+h {
		// Moving down: ensure end of entry is visible
		if cursorLineHeight <= h {
			// Entire entry can fit, scroll to show its end
			m.vp.YOffset = cursorYEnd - h + 1
		} else {
			// Entry is taller than screen, scroll to show its start (indicator)
			m.vp.YOffset = cursorYStart
		}
	}
}

func (m Model) enterVisual() (tea.Model, tea.Cmd) {
	m.inVisual = true
	m.visualStart = m.cursorLine
	m.visualEnd = m.cursorLine
	if m.vpReady {
		m.vp.SetContent(m.buildDetailContent())
	}
	return m, nil
}

func (m *Model) clampVisualRange(contentLines int) {
	if contentLines == 0 {
		m.visualStart = 0
		m.visualEnd = 0
		m.cursorLine = 0
		return
	}
	maxIdx := contentLines - 1
	if m.visualStart < 0 {
		m.visualStart = 0
	}
	if m.visualStart > maxIdx {
		m.visualStart = maxIdx
	}
	if m.visualEnd < 0 {
		m.visualEnd = 0
	}
	if m.visualEnd > maxIdx {
		m.visualEnd = maxIdx
	}
	if m.cursorLine < 0 {
		m.cursorLine = 0
	}
	if m.cursorLine > maxIdx {
		m.cursorLine = maxIdx
	}
}

func (m Model) updateVisual(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var contentLines int
	if lines, ok := m.logs[m.detailID]; ok {
		contentLines = len(lines)
	}

	// Clamp selection range to valid bounds
	m.clampVisualRange(contentLines)

	switch msg.String() {
	case "esc":
		m.inVisual = false
		m.visualStart = 0
		m.visualEnd = 0
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	case "v":
		m.inVisual = false
		m.visualStart = 0
		m.visualEnd = 0
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	case "y":
		text := m.getVisualSelection()
		m.inVisual = false
		m.visualStart = 0
		m.visualEnd = 0
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, copyToClipboard(text)
	case "j", "down":
		if contentLines > 0 && m.cursorLine < contentLines-1 {
			m.cursorLine++
			m.visualEnd = m.cursorLine
			m.followCursor()
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		return m, nil
	case "k", "up":
		if m.cursorLine > 0 {
			m.cursorLine--
			m.visualEnd = m.cursorLine
			m.followCursor()
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		return m, nil
	case "G":
		if contentLines > 0 {
			m.cursorLine = contentLines - 1
			m.visualEnd = m.cursorLine
			m.followCursor()
			if m.vpReady {
				m.vp.GotoBottom()
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		return m, nil
	case "g":
		m.cursorLine = 0
		m.visualEnd = m.cursorLine
		m.followCursor()
		if m.vpReady {
			m.vp.GotoTop()
			m.vp.SetContent(m.buildDetailContent())
		}
		return m, nil
	}
	return m, nil
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
	case "r":
		return m.handleReportKey()
	case "c":
		if !m.finished || m.IsChat {
			m.inPromptInput = true
			m.promptInput.SetValue("")
			m.promptInput.Focus()
			return m, textinput.Blink
		}
	case "i":
		m.inInfo = true
		return m, nil
	case "?", "F1":
		m.inHelp = true
		return m, nil
	case "a":
		m.inActivityLog = true
		m.initActivityVP()
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
		return m, nil
	case "N":
		if len(m.searchResults) > 0 {
			m.searchIdx = (m.searchIdx - 1 + len(m.searchResults)) % len(m.searchResults)
			m.jumpToSearchMatch()
		}
		return m, nil
	case "J":
		quarterPage := max(m.colBodyHeight()/4, 1)
		newRow := m.row + quarterPage
		if newRow >= len(col) {
			newRow = len(col) - 1
		}
		if len(col) > 0 {
			m.row = newRow
			m.scrollCursorIntoView()
		}
		return m, nil
	case "K":
		quarterPage := max(m.colBodyHeight()/4, 1)
		newRow := m.row - quarterPage
		if newRow < 0 {
			newRow = 0
		}
		m.row = newRow
		m.scrollCursorIntoView()
		return m, nil
	case "up", "k":
		if m.row > 0 {
			m.row--
			m.scrollCursorIntoView()
		}
		return m, nil
	case "down", "j":
		if m.row < len(col)-1 {
			m.row++
			m.scrollCursorIntoView()
		}
		return m, nil
	case "g":
		if len(col) > 0 {
			m.row = 0
			m.scrollOff[m.col] = 0
		}
		return m, nil
	case "G":
		if len(col) > 0 {
			m.row = len(col) - 1
			m.scrollCursorIntoView()
		}
		return m, nil
	case "ctrl+d":
		quarterPage := max(m.colBodyHeight()/4, 1)
		newRow := m.row + quarterPage
		if newRow >= len(col) {
			newRow = len(col) - 1
		}
		if len(col) > 0 {
			m.row = newRow
			m.scrollCursorIntoView()
		}
		return m, nil
	case "ctrl+u":
		quarterPage := max(m.colBodyHeight()/4, 1)
		newRow := m.row - quarterPage
		if newRow < 0 {
			newRow = 0
		}
		m.row = newRow
		m.scrollCursorIntoView()
		return m, nil
	case "left", "h":
		if m.col > 0 {
			m.col--
			m.row = 0
			m.scrollOff[m.col] = 0
		}
		return m, nil
	case "tab":
		m.col = (m.col + 1) % 6
		m.row = 0
		m.scrollOff[m.col] = 0
		return m, nil
	case "right", "l":
		if m.col < 5 {
			m.col++
			m.row = 0
			m.scrollOff[m.col] = 0
		}
		return m, nil
	case "enter":
		if m.finished && m.result != "" {
			m.inResult = true
			if m.vpReady {
				m.vp.SetContent(m.result)
				m.vp.GotoTop()
			}
			return m, nil
		}
		if m.row < len(col) {
			m.detailID = col[m.row].ID
			m.inDetail = true
			contentLines := len(m.logs[m.detailID])
			if contentLines > 0 {
				m.cursorLine = contentLines - 1
			} else {
				m.cursorLine = 0
			}
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
				m.vp.GotoBottom()
				m.followCursor() // Ensure indicator is visible for multi-line entries
			}
			if !m.mouseEnabled {
				m.mouseEnabled = true
				return m, enableMouseCmd()
			}
		}
	case "m":
		m.mouseManuallyEnabled = !m.mouseManuallyEnabled
		m.mouseEnabled = m.mouseManuallyEnabled
		if m.mouseEnabled {
			return m, enableMouseCmd()
		}
		return m, disableMouseCmd()
	case "M":
		m.inMemory = true
		m.loadMemoryContent()
		return m, nil
	}
	return m, nil
}

func (m Model) updateResult(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "enter":
		m.inResult = false
		return m, nil
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "ctrl+c":
		return m.handleCtrlC()
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m Model) handleReportKey() (tea.Model, tea.Cmd) {
	if m.finished {
		select {
		case m.ReportCh <- struct{}{}:
			m.statusText = dimStyle.Render("Generating report...")
		default:
		}
	}
	return m, nil
}

func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.wrapUpRequested {
		return m, tea.Quit
	}
	if m.finished {
		return m, tea.Quit
	}
	m.wrapUpRequested = true
	select {
	case m.WrapUpCh <- struct{}{}:
	default:
	}
	m.statusText = wrapUpStyle.Render("⏹ Finishing active tasks — Ctrl+C to force quit")
	return m, nil
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "h":
		if m.confirmChoice > 0 {
			m.confirmChoice--
		}
	case "right", "l", "tab":
		if m.confirmChoice < 2 {
			m.confirmChoice++
		}
	case "enter":
		switch m.confirmChoice {
		case 1: // Yes
			m.inConfirm = false
			return m.handleCtrlC()
		case 2: // Force
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
		m.confirmChoice = 1
		m.inConfirm = false
		return m.handleCtrlC()
	case "f":
		m.confirmChoice = 2
		return m, tea.Quit
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
			if m.IsChat {
				m.finished = false
				m.result = ""
				m.statusText = "Waiting for coordinator..."
			}
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

func (m Model) updateHelp(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "?", "F1", "enter":
		m.inHelp = false
		return m, nil
	case "ctrl+c":
		m.inHelp = false
		return m.handleCtrlC()
	}
	return m, nil
}

func (m Model) updateActivityLog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "a", "enter":
		m.inActivityLog = false
		return m, nil
	case "ctrl+c":
		m.inActivityLog = false
		return m.handleCtrlC()
	case "g":
		if m.vpReady {
			m.vp.GotoTop()
		}
	case "G":
		if m.vpReady {
			m.vp.GotoBottom()
		}
	case "j", "down":
		if m.vpReady {
			m.vp, _ = m.vp.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
	case "k", "up":
		if m.vpReady {
			m.vp, _ = m.vp.Update(tea.KeyMsg{Type: tea.KeyUp})
		}
	case " ":
		if m.vpReady {
			m.vp, _ = m.vp.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		}
	case "b":
		if m.vpReady {
			m.vp, _ = m.vp.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		}
	}
	return m, nil
}

func (m Model) activityLogView() string {
	if m.width == 0 {
		return "Initialising..."
	}

	var b strings.Builder

	// Header
	header := headerStyle.Render("─── ACTIVITY LOG ──────────────────────────────") + " " +
		dimStyle.Render("[j/k] scroll [esc/q/a] close")
	b.WriteString(header)
	b.WriteString("\n\n")

	// Initialize viewport if needed (defensive: should already be init from Update)
	if !m.vpReady {
		// Viewport not ready - show placeholder
		b.WriteString(dimStyle.Render("Activity log initializing..."))
		b.WriteString("\n")
		footer := dimStyle.Render("──────────────────────────────────────────────────────────────────────────────")
		b.WriteString(footer)
		return b.String()
	}

	// Update viewport content
	m.vp.SetContent(m.formatActivityLogContent())

	// Viewport content
	b.WriteString(m.vp.View())
	b.WriteString("\n")

	// Footer
	footer := dimStyle.Render("──────────────────────────────────────────────────────────────────────────────")
	b.WriteString(footer)

	return b.String()
}

func (m *Model) initActivityVP() {
	w := m.width
	h := m.height - 4 // account for header and footer
	if h < 3 {
		h = 3
	}
	m.vp = viewport.New(w, h)
	m.vp.SetContent(m.formatActivityLogContent())
	m.vp.GotoBottom()
	m.vpReady = true
}

func (m *Model) formatActivityLogContent() string {
	if len(m.recentLogs) == 0 {
		return dimStyle.Render("No activity log entries yet.")
	}

	var b strings.Builder
	for _, entry := range m.recentLogs {
		// Format each entry with styling similar to CLI output
		b.WriteString(entry)
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) executeSearch(query string) []*team.TodoItem {
	q := strings.ToLower(query)
	var results []*team.TodoItem
	for col := 0; col < 6; col++ {
		items := m.colItems(col)
		for _, item := range items {
			if strings.Contains(strings.ToLower(item.Agent), q) ||
				strings.Contains(strings.ToLower(item.Desc), q) ||
				strings.Contains(strings.ToLower(item.Detail), q) ||
				strings.Contains(strings.ToLower(item.Source), q) {
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
	for col := 0; col < 6; col++ {
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
	}
	return m.columnsView()
}

func (m Model) resultView() string {
	header := headerStyle.Render("Result") + "\n" + dimStyle.Render("esc/enter back · q quit") + "\n\n"
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
	wrapped := utils.WrapLine(m.prompt, textW, 5)
	var content strings.Builder
	content.WriteString(promptStyle.Render(labelPlain))
	if len(wrapped.Lines) > 0 {
		content.WriteString(wrapped.Lines[0])
		for _, line := range wrapped.Lines[1:] {
			content.WriteString("\n")
			content.WriteString(strings.Repeat(" ", labelRunes))
			content.WriteString(line)
		}
	}
	return promptBoxStyle.Width(innerW).Render(content.String())
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
	wrapped := utils.WrapLine(m.prompt, textW, 5)
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
	// Prefer showing the most recent in_progress task's name so the status
	// bar reflects what is happening *now*, not whatever event arrived last
	// (which may be for a task that has already completed in the background).
	if !m.finished {
		for i := len(m.tasks) - 1; i >= 0; i-- {
			if t := m.tasks[i]; t != nil && t.Status == team.TaskInProgress {
				text = "  " + agentStyle.Render(t.Agent) + dimStyle.Render("  "+t.Desc)
				return utils.TruncateLine(ansi.Strip(text), m.width-3)
			}
		}
	}
	if text == "" {
		if m.finished {
			return dimStyle.Render("  ✓ Done")
		}
		return dimStyle.Render("  ⟳ Initialising…")
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
	plain = utils.TruncateLine(plain, maxW)
	return "  " + plain
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
			b.WriteString(dimStyle.Render(line))
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
	if w < 60 {
		warning := infoBoxStyle.Render("Terminal too narrow\n\nResize to at least 60 columns to view task status.")
		return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center, warning)
	}

	widget := m.renderPromptWidget(w)
	progress := m.renderProgressBar(w)
	statusArea := m.renderStatusArea(w)
	activityFeed := m.renderActivityFeed(w)
	statusH := m.statusAreaHeight()
	promptH := m.promptWidgetHeight()
	feedH := m.countFeedLines()

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
	bodyH := m.height - promptH - 1 - progressH - statusH - 1 - feedTotal - 2
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
	div := dimStyle.Render("│")

	body := lipgloss.JoinHorizontal(lipgloss.Top, c0, div, c1, div, c2, div, c3, div, c4, div, c5)
	prefix := widget + "\n"
	if progress != "" {
		prefix += progress + "\n"
	}
	if feedH > 0 {
		return prefix + statusArea + "\n" + activityFeed + "\n" + body + "\n\n" + m.footer()
	}
	return prefix + statusArea + "\n" + body + "\n\n" + m.footer()
}

func (m Model) isCompact() bool {
	return m.width >= 60 && m.width < 80
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
	div := dimStyle.Render("│")
	columns := make([]string, 0, len(groups))
	for _, group := range groups {
		columns = append(columns, m.renderCompactCol(group.title, group.cols, colW, bodyH))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, columns[0], div, columns[1], div, columns[2])
	prefix := widget + "\n"
	if progress != "" {
		prefix += progress + "\n"
	}
	if feedH > 0 {
		return prefix + statusArea + "\n" + activityFeed + "\n" + body + "\n\n" + m.footer()
	}
	return prefix + statusArea + "\n" + body + "\n\n" + m.footer()
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
		case team.TaskError, team.TaskBlocked:
			failed++
		}
	}
	barWidth := 20
	if width < 55 {
		barWidth = 10
	}
	filled := done * barWidth / len(m.tasks)
	bar := doneIcon.Render(strings.Repeat("█", filled)) + dimStyle.Render(strings.Repeat("░", barWidth-filled))
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
		sb.WriteString(headerStyle.Render(utils.TruncateLine(plainTitle, width)) + "\n")
	} else {
		sb.WriteString(dimStyle.Render(utils.TruncateLine(plainTitle, width)) + "\n")
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
	truncated := utils.TruncateLine(plainTitle, width)
	var sb strings.Builder
	if focused {
		sb.WriteString(headerStyle.Render(truncated) + "\n")
	} else {
		sb.WriteString(dimStyle.Render(truncated) + "\n")
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
	icon, iconSt := taskIconStyle(item.Status)
	sourceLabel := sourceTag(item.Source)
	agentLine := utils.TruncateLine(item.Agent, width-3-lipgloss.Width(sourceLabel))

	if item.ID == team.CoordTodoID {
		coordLabel := dimStyle.Render("(coordinator)")
		agentLine = utils.TruncateLine(item.Agent, width-len("(coordinator)")-4)

		var lines []string
		if selected {
			line1 := "▶ " + agentStyle.Render(agentLine) + " " + coordLabel
			lines = []string{line1}
			lines = append(lines, formatDescLines("coordinating", width, 1, selected)...)
		} else {
			line1 := iconSt.Render(icon+" ") + agentStyle.Render(agentLine) + " " + coordLabel
			lines = []string{line1}
			lines = append(lines, formatDescLines("coordinating", width, 1, false)...)
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
		line1 := "▶ " + agentStyle.Render(agentLine) + sourceLabel
		lines = []string{line1}
		lines = append(lines, formatDescLines(item.Desc, width, maxDescLines, true)...)
	} else {
		line1 := iconSt.Render(icon+" ") + agentStyle.Render(agentLine) + sourceLabel + matchIndicator
		lines = []string{line1}
		lines = append(lines, formatDescLines(item.Desc, width, maxDescLines, false)...)
	}

	if len(item.Skills) > 0 {
		skillLine := utils.TruncateLine("["+strings.Join(item.Skills, " · ")+"]", width-2)
		lines = append(lines, skillStyle.Render("  "+skillLine))
	}

	if item.Status == team.TaskError && item.Detail != "" {
		errLine := utils.TruncateLine(item.Detail, width-4)
		lines = append(lines, errorIcon.Render("  ✗ "+errLine))
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

func formatDescLines(desc string, width, maxLines int, selected bool) []string {
	wrapped := utils.WrapLine(desc, width-2, maxLines)
	if len(wrapped.Lines) == 0 {
		return nil
	}
	result := make([]string, len(wrapped.Lines))
	for i, line := range wrapped.Lines {
		if selected {
			result[i] = "  " + line
		} else {
			result[i] = dimStyle.Render("  " + line)
		}
	}
	return result
}

func sourceTag(source string) string {
	switch source {
	case team.TaskSourceAgent:
		return dimStyle.Render(" [A]")
	case team.TaskSourceSubagent:
		return dimStyle.Render(" [S]")
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

func taskIconStyle(s team.TaskStatus) (string, lipgloss.Style) {
	switch s {
	case team.TaskInProgress:
		return "◑", progressIcon
	case team.TaskVerifying:
		return "◔", progressIcon
	case team.TaskDone:
		return "●", doneIcon
	case team.TaskError:
		return "✗", errorIcon
	case team.TaskBlocked:
		return "⚠", errorIcon
	case team.TaskSkipped:
		return "—", skippedIcon
	case team.TaskPlanned:
		return "◎", dimStyle
	case team.TaskPaused:
		return "◐", pausedIcon
	}
	return "○", pendingIcon
}

func (m Model) footer() string {
	if m.inDetail {
		if m.inVisual {
			start := min(m.visualStart, m.visualEnd)
			end := max(m.visualStart, m.visualEnd)
			lineCount := end - start + 1
			return visualLabel.Render("-- VISUAL --") + " " +
				footerStyle.Render(fmt.Sprintf("%d line%s selected · ", lineCount, pluralS(lineCount))) +
				boldStyle.Render("y") + footerStyle.Render(" copy · ") +
				boldStyle.Render("v/esc") + footerStyle.Render(" cancel · ") +
				boldStyle.Render("j/k") + footerStyle.Render(" extend")
		}
		return footerStyle.Render("J/K/ctrl+d/u ↑↓ scroll · v visual · esc back")
	}
	if m.inInfo {
		return footerStyle.Render("i/esc close · ↑↓ scroll")
	}
	if m.inHelp {
		return footerStyle.Render("?/esc close")
	}
	if m.inSearch {
		return footerStyle.Render("enter search · esc cancel")
	}
	if len(m.searchResults) > 0 {
		return footerStyle.Render(fmt.Sprintf("n/N next/prev match (%d/%d) · / search · i info · esc clear · g/G top/bot · J/K/ctrl+d/u scroll · ↑↓ j/k · enter detail · q quit", m.searchIdx+1, len(m.searchResults)))
	}
	if m.finished {
		if m.IsChat {
			return footerStyle.Render("g/G top/bot · J/K/ctrl+d/u scroll · / search · r report · i info · c chat · ↑↓ j/k · enter detail · q quit")
		}
		return footerStyle.Render("g/G top/bot · J/K/ctrl+d/u scroll · / search · r report · i info · ↑↓ j/k · enter detail · q quit")
	}
	return footerStyle.Render("g/G top/bot · J/K/ctrl+d/u scroll · / search · i info · c prompt · ? help · ↑↓ j/k · enter detail · esc quit")
}

func (m Model) helpView() string {
	box := infoBoxStyle.Render(`hufu TUI — keyboard reference

Columns (dashboard)
  h / l  ←/→        switch column
  tab               cycle through all 6 columns
  j / k  ↑/↓        move cursor in column
  g / G             first / last item
  ctrl+d / ctrl+u   half-page down / up
  enter             open detail view for the focused task

Search
  /                 open search dialog
  n / N             next / previous match

Actions
  c                 inject prompt / chat
  i                 show team info panel
  a                 full-screen activity log
  M                 memory (STM/LTM) view
  m                 toggle mouse support
  r                 generate report (only when finished)
  ?  or  F1         this help screen
  q                 quit (only when finished)
  esc               open quit confirmation
  ctrl+c            request wrap-up (1st) / force quit (2nd)

Detail view
  j/k ↑/↓           scroll one line
  g / G             top / bottom
  v                 enter VISUAL mode
  y                 yank selection to clipboard (VISUAL only)
  esc / backspace   return to columns
`)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m Model) confirmView() string {
	noLabel := " No "
	yesLabel := " Yes "
	forceLabel := " Force "
	noStyled := confirmNormalStyle.Render(noLabel)
	yesStyled := confirmNormalStyle.Render(yesLabel)
	forceStyled := confirmNormalStyle.Render(forceLabel)
	switch m.confirmChoice {
	case 0:
		noStyled = confirmHighlightStyle.Render(noLabel)
	case 1:
		yesStyled = confirmHighlightStyle.Render(yesLabel)
	case 2:
		forceStyled = confirmHighlightStyle.Render(forceLabel)
	}
	buttons := noStyled + "  " + yesStyled + "  " + forceStyled
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
			fmt.Fprintf(&b, " (model: %s)", info.MemoryModel)
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

	if info.SSHSessions > 0 {
		b.WriteString(boldStyle.Render("SSH:     "))
		fmt.Fprintf(&b, "%d active session(s)\n", info.SSHSessions)
	}

	b.WriteString("\n" + dimStyle.Render("esc close"))

	content := b.String()
	dialog := infoBoxStyle.Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func (m *Model) loadMemoryContent() {
	var b strings.Builder
	b.WriteString(headerStyle.Render("─── Memory ───"))
	b.WriteString("\n\n")

	ws := m.teamInfo.Workspace

	if ws != "" {
		data, err := os.ReadFile(filepath.Join(ws, "stm.md"))
		if err == nil && len(data) > 0 {
			content := strings.TrimSpace(string(data))
			if content != "" {
				b.WriteString(boldStyle.Render("Short-term Memory (stm.md)"))
				b.WriteString("\n\n")
				b.WriteString(renderMemory(content))
				b.WriteString("\n\n")
			} else {
				b.WriteString(dimStyle.Render("Short-term memory is empty."))
				b.WriteString("\n\n")
			}
		} else {
			b.WriteString(dimStyle.Render("Short-term memory is empty."))
			b.WriteString("\n\n")
		}
	}

	if ws != "" && m.teamInfo.TeamName != "" {
		data, err := os.ReadFile(filepath.Join(ws, fmt.Sprintf("ltm-%s.md", m.teamInfo.TeamName)))
		if err == nil && len(data) > 0 {
			content := strings.TrimSpace(string(data))
			if content != "" {
				b.WriteString(boldStyle.Render("Long-term Memory (ltm.md)"))
				b.WriteString("\n\n")
				b.WriteString(renderMemory(content))
				b.WriteString("\n\n")
			} else {
				b.WriteString(dimStyle.Render("Long-term memory is empty."))
				b.WriteString("\n\n")
			}
		} else {
			b.WriteString(dimStyle.Render("Long-term memory is empty."))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(dimStyle.Render("esc back"))

	w := m.width - 4
	if w < 10 {
		w = 10
	}
	h := m.height - 2
	if h < 3 {
		h = 3
	}
	m.memoryVP = viewport.New(w, h)
	m.memoryVP.SetContent(b.String())
	m.memoryVP.GotoTop()
	m.memoryReady = true
}

func renderMemory(content string) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			b.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			b.WriteString(boldStyle.Render(trimmed))
		} else if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			b.WriteString("  " + dimStyle.Render(trimmed))
		} else {
			runes := []rune(trimmed)
			if len(runes) > 80 {
				trimmed = string(runes[:80]) + "..."
			}
			b.WriteString(dimStyle.Render(trimmed))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) memoryView() string {
	if !m.memoryReady {
		return ""
	}
	footer := footerStyle.Render("j/k ↑↓ scroll · esc back")
	return m.memoryVP.View() + "\n" + footer
}

func (m Model) updateMemory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.inMemory = false
		m.memoryReady = false
		return m, nil
	case "ctrl+c":
		return m.handleCtrlC()
	case "q":
		if m.finished {
			return m, tea.Quit
		}
	case "g":
		if m.memoryReady {
			m.memoryVP.GotoTop()
		}
		return m, nil
	case "G":
		if m.memoryReady {
			m.memoryVP.GotoBottom()
		}
		return m, nil
	}
	if m.memoryReady {
		var cmd tea.Cmd
		m.memoryVP, cmd = m.memoryVP.Update(msg)
		return m, cmd
	}
	return m, nil
}

// ── Detail view ───────────────────────────────────────────────────────────────

func (m Model) detailView() string {
	item := m.findTask(m.detailID)
	if item == nil {
		return "task not found — press esc"
	}

	header := m.renderDetailHeader(item)
	if !m.vpReady {
		return header
	}
	footer := m.footer()
	return header + "\n" + m.vp.View() + "\n" + footer
}

func (m Model) renderDetailHeader(item *team.TodoItem) string {
	icon, iconSt := taskIconStyle(item.Status)

	sep := dimStyle.Render(strings.Repeat("─", m.width))
	taskNum := ""
	for i, t := range m.tasks {
		if t.ID == item.ID {
			taskNum = fmt.Sprintf(" #%d ", i+1)
			break
		}
	}
	titleLine := headerStyle.Render("─── Task" + taskNum + "───")

	agentLine := iconSt.Render(icon) + " " + agentStyle.Render(item.Agent)
	if item.ID == team.CoordTodoID {
		agentLine += " " + dimStyle.Render("(coordinator)")
	}

	modelLine := dimStyle.Render("model: ")
	if item.Model != "" {
		modelLine += dimStyle.Render(item.Model)
	} else {
		modelLine += dimStyle.Render("—")
	}

	var skillsLine string
	if len(item.Skills) > 0 {
		skillsLine = skillStyle.Render("skills: " + strings.Join(item.Skills, " · "))
	}

	var injectedLine string
	if len(item.InjectedSkills) > 0 {
		injectedLine = dimStyle.Render("auto: ") + skillStyle.Render(strings.Join(item.InjectedSkills, " · "))
	}

	var loadedLine string
	if len(item.LoadedSkills) > 0 {
		loadedLine = dimStyle.Render("used: ") + doneStyle.Render(strings.Join(item.LoadedSkills, " · "))
	}

	descWrapped := utils.WrapLine(item.Desc, m.width-4, 4)
	var descLines []string
	for _, l := range descWrapped.Lines {
		descLines = append(descLines, dimStyle.Render("  "+l))
	}
	descBlock := strings.Join(descLines, "\n")

	var parts []string
	parts = append(parts, titleLine)
	parts = append(parts, agentLine+"  "+modelLine)
	if skillsLine != "" {
		parts = append(parts, skillsLine)
	}
	if injectedLine != "" {
		parts = append(parts, injectedLine)
	}
	if loadedLine != "" {
		parts = append(parts, loadedLine)
	}
	parts = append(parts, descBlock)

	if item.Source != "" && item.Source != team.TaskSourceCoordinator {
		parts = append(parts, sourceDetailTag(item.Source))
	}
	if item.ParentID != "" {
		parent := m.findTask(item.ParentID)
		if parent != nil {
			parentDesc := utils.TruncateLine(parent.Agent+": "+parent.Desc, m.width-20)
			parts = append(parts, dimStyle.Render("parent: "+item.ParentID+". "+parentDesc))
		} else {
			parts = append(parts, dimStyle.Render("parent: #"+item.ParentID))
		}
	}

	var subtaskLines string
	for _, t := range m.tasks {
		if t.ParentID == item.ID {
			subtaskLines += renderSubtaskLine(t, m.width)
		}
	}
	if subtaskLines != "" {
		parts = append(parts, dimStyle.Render("─── Subtasks ───"))
		parts = append(parts, subtaskLines)
	}

	parts = append(parts, sep)
	return strings.Join(parts, "\n")
}

func sourceDetailTag(source string) string {
	switch source {
	case team.TaskSourceAgent:
		return agentStyle.Render("[agent]")
	case team.TaskSourceSubagent:
		return agentStyle.Render("[subagent]")
	default:
		return ""
	}
}

func renderSubtaskLine(t *team.TodoItem, width int) string {
	icon, _ := taskIconStyle(t.Status)
	tag := sourceTag(t.Source)
	agentLine := utils.TruncateLine(t.Agent, width-8)
	descWrapped := utils.WrapLine(t.Desc, width-8, 2)
	parts := []string{agentStyle.Render(agentLine) + tag}
	for _, dl := range descWrapped.Lines {
		parts = append(parts, dimStyle.Render(dl))
	}
	return fmt.Sprintf("  %s %s\n", icon, strings.Join(parts, " "))
}

// vpHeight is the number of lines available for the detail viewport.
func (m Model) vpHeight() int {
	descLines := (m.width - 4) / 80
	if descLines < 2 {
		descLines = 2
	}
	headerLines := 6 + descLines // title(1) + agent+model(1) + skills(1) + desc(N) + sep(1) + buffer(2)
	if m.detailID != "" {
		if item := m.findTask(m.detailID); item != nil {
			if len(item.InjectedSkills) > 0 {
				headerLines++
			}
			if len(item.LoadedSkills) > 0 {
				headerLines++
			}
		}
	}
	h := m.height - headerLines
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
	width := m.width
	if width < 20 {
		width = 20
	}
	if m.inVisual {
		return m.buildVisualContent(lines, width)
	}
	var result strings.Builder
	for i, entry := range lines {
		if i > 0 {
			result.WriteString("\n")
		}
		wrapped := wrapText(entry, width-2) // leave space for indicator
		if i == m.cursorLine {
			result.WriteString(cursorStyle.Render("› ") + wrapped)
		} else {
			result.WriteString("  " + wrapped)
		}
	}
	return result.String()
}

func (m Model) buildVisualContent(lines []string, width int) string {
	start := min(m.visualStart, m.visualEnd)
	end := max(m.visualStart, m.visualEnd)
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	var result strings.Builder
	for i, entry := range lines {
		if i > 0 {
			result.WriteString("\n")
		}
		wrapped := wrapText(entry, width-2)
		var line string
		if i == m.cursorLine {
			line = cursorStyle.Render("› ") + wrapped
		} else {
			line = "  " + wrapped
		}

		if i >= start && i <= end {
			result.WriteString(visualStyle.Render(line))
		} else {
			result.WriteString(line)
		}
	}
	return result.String()
}

func wrapLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var result strings.Builder
	line := strings.Builder{}
	pendingANSI := strings.Builder{}
	lineVisible := 0

	flushLine := func() {
		if line.Len() > 0 {
			if pendingANSI.Len() > 0 {
				line.WriteString(pendingANSI.String())
				pendingANSI.Reset()
			}
			result.WriteString(line.String())
			result.WriteByte('\n')
			line.Reset()
			lineVisible = 0
		}
	}

	i := 0
	for i < len(s) {
		// Capture any pending ANSI sequences
		if s[i] == '\x1b' {
			j := i
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++
			}
			pendingANSI.WriteString(s[i:j])
			i = j
			continue
		}

		// Collect the next word (non-whitespace sequence)
		wordStart := i
		wordLen := 0
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\x1b' {
			_, size := utf8.DecodeRuneInString(s[i:])
			wordLen++
			i += size
		}
		// Count visible characters (runes)
		runeCount := 0
		for j := wordStart; j < i; {
			_, size := utf8.DecodeRuneInString(s[j:])
			runeCount++
			j += size
		}

		// Check if word fits on current line
		if lineVisible > 0 && lineVisible+1+wordLen > width {
			flushLine()
		}

		// Emit word (possibly split if wider than width)
		for runeCount > 0 {
			chunkLen := width - lineVisible
			if chunkLen <= 0 {
				flushLine()
				chunkLen = width
			}
			if chunkLen > runeCount {
				chunkLen = runeCount
			}

			extracted := chunkLen

			// Extract chunk from current position in word
			var chunkBuilder strings.Builder
			pos := wordStart
			for chunkLen > 0 {
				r, size := utf8.DecodeRuneInString(s[pos:])
				chunkBuilder.WriteRune(r)
				pos += size
				chunkLen--
			}

			// Flush pending ANSI before first content on line
			if lineVisible == 0 && pendingANSI.Len() > 0 {
				line.WriteString(pendingANSI.String())
				pendingANSI.Reset()
			}

			line.WriteString(chunkBuilder.String())
			lineVisible += extracted
			runeCount -= extracted
			wordStart = pos
		}

		// Add space after word (if we stopped at whitespace)
		if i < len(s) && s[i] != '\x1b' && (s[i] == ' ' || s[i] == '\t') {
			if line.Len() > 0 {
				line.WriteByte(s[i])
				lineVisible++
			}
			i++
		}
	}

	if line.Len() > 0 {
		result.WriteString(line.String())
	}

	return result.String()
}

func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	parts := strings.Split(s, "\n")
	var result strings.Builder
	for pi, part := range parts {
		if pi > 0 {
			result.WriteString("\n")
		}
		result.WriteString(wrapLine(part, width))
	}
	return result.String()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) getVisualSelection() string {
	lines := m.logs[m.detailID]
	if len(lines) == 0 {
		return ""
	}
	start := min(m.visualStart, m.visualEnd)
	end := max(m.visualStart, m.visualEnd)
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	if start > end {
		start, end = end, start
	}
	selected := lines[start : end+1]
	return strings.Join(selected, "\n")
}

func (m Model) mapRenderedLineToLogIndex(renderedLine int, width int) int {
	lines := m.logs[m.detailID]
	currRenderedLine := 0
	for i, entry := range lines {
		wrapped := wrapText(entry, width-2)
		linesInWrapped := len(strings.Split(wrapped, "\n"))
		if linesInWrapped == 0 {
			linesInWrapped = 1
		}
		if renderedLine >= currRenderedLine && renderedLine < currRenderedLine+linesInWrapped {
			return i
		}
		currRenderedLine += linesInWrapped
	}
	return -1
}

func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		if text == "" {
			return copySuccessMsg{Lines: 0}
		}
		osc52 := "\x1b]52;c;" + encodeOSC52(text) + "\x1b\\"
		fmt.Fprint(os.Stderr, osc52)
		lines := strings.Count(text, "\n") + 1
		return copySuccessMsg{Lines: lines}
	}
}

func encodeOSC52(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (m Model) colItems(col int) []*team.TodoItem {
	var out []*team.TodoItem
	for _, t := range m.tasks {
		switch {
		case col == 0 && t.Status == team.TaskPending:
			out = append(out, t)
		case col == 1 && t.Status == team.TaskPlanned:
			out = append(out, t)
		case col == 2 && (t.Status == team.TaskInProgress || t.Status == team.TaskPaused || t.Status == team.TaskVerifying):
			out = append(out, t)
		case col == 3 && t.Status == team.TaskDone:
			out = append(out, t)
		case col == 4 && t.Status == team.TaskSkipped:
			out = append(out, t)
		case col == 5 && (t.Status == team.TaskError || t.Status == team.TaskBlocked):
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
	if toolName == "bash" {
		args = strings.TrimSpace(args)
		lines := strings.Split(args, "\n")
		indented := make([]string, len(lines))
		for i, l := range lines {
			indented[i] = dimStyle.Render("  " + l)
		}
		return toolCallStyle.Render("⟹ "+toolName) + "\n" + strings.Join(indented, "\n")
	}
	if len(args) > 4000 {
		r := []rune(args)
		if len(r) > 4000 {
			args = string(r[:4000]) + "…"
		}
	}
	return toolCallStyle.Render("⟹ "+toolName) + "\n" + dimStyle.Render("  "+args)
}

func RenderToolResult(toolName, result string) string {
	if len(result) > 4000 {
		r := []rune(result)
		if len(r) > 4000 {
			result = string(r[:4000]) + "…"
		}
	}
	return toolResStyle.Render("✓ "+toolName) + "\n" + dimStyle.Render("  "+result)
}

// RenderText returns a formatted text-delta log line.
func RenderText(text string) string {
	return textLogStyle.Render("💬 " + strings.TrimSpace(text))
}
