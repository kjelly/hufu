package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
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

type FinishedMsg struct {
	Result *team.RunResult
}

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
	PTYEnabled     bool
	Decisions      []team.DecisionIndexEntry
	HufuBinary     string
}

type TeamInfoMsg struct{ Info TeamInfo }

type DecisionStateMsg struct{ Decisions []team.DecisionIndexEntry }

// OperatorSnapshotMsg refreshes the shared read-only snapshot used by both
// inspect overview and the TUI summary strip.
type OperatorSnapshotMsg struct{ Snapshot operator.OperatorSnapshot }

type OperatorEvidenceDetail struct {
	Available      bool
	ManifestStatus string
	Verdict        string
	Acceptance     string
	Requirements   int
	Artifacts      int
	Findings       int
}

type OperatorPromotionDetail struct {
	ID          string
	Type        string
	Status      string
	TargetPath  string
	SourceCount int
}

type OperatorDetailsMsg struct {
	Evidence         OperatorEvidenceDetail
	Promotions       []OperatorPromotionDetail
	PromotionStatus  string
	PromotionTotal   int
	PromotionLimited bool
}

type SSHSessionsMsg struct{ Count int }

// TerminalSessionMsg maps a task to the PTY session created by its terminal
// tool call. It lets the detail view attach without asking the operator to
// copy a session ID from logs.
type TerminalSessionMsg struct {
	TodoID    string
	SessionID string
}

type terminalAttachFinishedMsg struct{ Err error }

type WrapUpMsg struct{}

type AskUserCancelMsg struct{}

type detailRefreshMsg struct{}
type spinnerTickMsg struct{}

type copySuccessMsg struct{ Lines int }

var defaultSpinnerEnabled = true
var defaultCompactMode bool

// SetSpinnerEnabled configures the default for newly created TUI models.
func SetSpinnerEnabled(enabled bool) { defaultSpinnerEnabled = enabled }

// SetCompactMode configures the default layout for newly created TUI models.
func SetCompactMode(enabled bool) { defaultCompactMode = enabled }

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

	inMemory      bool
	memoryVP      viewport.Model
	memoryReady   bool
	inOperator    bool
	operatorVP    viewport.Model
	operatorReady bool

	inConfirm     bool // showing quit confirmation dialog
	confirmChoice int  // 0=no 1=yes 2=force

	width                    int
	height                   int
	finished                 bool
	IsChat                   bool
	runResult                *team.RunResult
	statusText               string // current status shown in the status bar
	spinnerFrame             int
	spinnerEnabled           bool
	forceCompact             bool
	themeMode                ThemeMode
	effectiveTheme           ThemeMode
	displayPreset            DisplayPreset
	noColor                  bool
	owner                    bool
	themeContext             context.Context
	themeDetector            func(context.Context) ThemeMode
	themePoll                time.Duration
	themeGeneration          uint64
	styles                   styleSet
	operatorSummary          operator.OperatorSummary
	operatorScope            operator.ResolvedScope
	operatorSnapshot         operator.OperatorSnapshot
	operatorEvidence         OperatorEvidenceDetail
	operatorPromotions       []OperatorPromotionDetail
	operatorPromotionStatus  string
	operatorPromotionTotal   int
	operatorPromotionLimited bool
	hasOperatorSummary       bool
	unread                   map[string]int
	result                   string // final coordinator answer shown when finished

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

	inVisual    bool              // VISUAL mode active in detail view
	cursorLine  int               // current line index within detail logs
	visualStart int               // selection start line index
	visualEnd   int               // selection end line index
	terminals   map[string]string // todoID → current PTY session ID
}

func enableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.EnableMouseCellMotion() }
}

func disableMouseCmd() tea.Cmd {
	return func() tea.Msg { return tea.DisableMouse() }
}

// New creates a fresh model with the user's original prompt shown at the top.
func New(prompt string, teamInfo TeamInfo) Model {
	return NewWithOptions(prompt, teamInfo, Options{
		Theme: ThemeAuto, DisplayPreset: DisplayDefault,
		Spinner: defaultSpinnerEnabled && os.Getenv("NO_SPINNER") == "",
		Compact: defaultCompactMode, Owner: true,
	})
}

// NewWithOptions creates a model whose presentation preferences are isolated
// from every other model in the process.
func NewWithOptions(prompt string, teamInfo TeamInfo, options Options) Model {
	options = options.normalized()
	teamInfo.Decisions = team.RedactedDecisionIndexEntries(teamInfo.Decisions)
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "Type additional prompt..."
	ti.CharLimit = 500

	si := textinput.New()
	si.Prompt = "/"
	si.Placeholder = "Search tasks..."
	si.CharLimit = 200
	if options.DisplayPreset == DisplayEpaper {
		_ = ti.Cursor.SetMode(cursor.CursorStatic)
		_ = si.Cursor.SetMode(cursor.CursorStatic)
	}

	m := Model{
		prompt:         prompt,
		logs:           make(map[string][]string),
		terminals:      make(map[string]string),
		unread:         make(map[string]int),
		promptInput:    ti,
		searchInput:    si,
		PromptInjectCh: make(chan string, 16),
		WrapUpCh:       make(chan struct{}, 2),
		ReportCh:       make(chan struct{}, 1),
		teamInfo:       teamInfo,
		IsChat:         teamInfo.IsChat,
		spinnerEnabled: options.Spinner && os.Getenv("NO_SPINNER") == "",
		forceCompact:   options.Compact,
		themeMode:      options.Theme,
		effectiveTheme: effectiveTheme(options.Theme),
		displayPreset:  options.DisplayPreset,
		noColor:        options.NoColor,
		owner:          options.Owner,
		themeContext:   options.Context,
		themeDetector:  options.ThemeDetector,
		themePoll:      options.ThemePoll,
		styles:         newStyleSet(options.Theme, options.NoColor),
	}

	if m.IsChat && prompt == "" {
		m.inPromptInput = true
		m.promptInput.Focus()
	}

	return m
}

func (m Model) Init() tea.Cmd {
	var commands []tea.Cmd
	if m.IsChat && m.inPromptInput && m.displayPreset != DisplayEpaper {
		commands = append(commands, textinput.Blink)
	}
	if command := m.watchThemeCmd(); command != nil {
		commands = append(commands, command)
	}
	return tea.Batch(commands...)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case AskUserMsg:
		var cmd tea.Cmd
		m.ask, cmd = initAskUser(msg, m.width)
		if m.displayPreset == DisplayEpaper {
			cmd = m.ask.ti.Cursor.SetMode(cursor.CursorStatic)
		}
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
		if m.operatorReady {
			m.operatorVP.Width = max(msg.Width-4, 10)
			m.operatorVP.Height = max(msg.Height-2, 3)
			if m.inOperator {
				offset := m.operatorVP.YOffset
				m.operatorVP.SetContent(m.buildOperatorContent())
				m.operatorVP.SetYOffset(offset)
			}
		}
		m.clampScroll()
		if command := m.probeThemeCmd(); command != nil {
			return m, command
		}

	case themeDetectedMsg:
		if msg.generation != m.themeGeneration || m.themeMode != ThemeAuto {
			return m, nil
		}
		if msg.mode != m.effectiveTheme {
			m.effectiveTheme = msg.mode
			m.styles = newStyleSet(msg.mode, m.noColor)
			return m, tea.Batch(tea.ClearScreen, m.watchThemeCmd())
		}
		return m, m.watchThemeCmd()

	case themeProbeMsg:
		if msg.generation == m.themeGeneration && m.themeMode == ThemeAuto && msg.mode != m.effectiveTheme {
			m.effectiveTheme = msg.mode
			m.styles = newStyleSet(msg.mode, m.noColor)
			return m, tea.ClearScreen
		}

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
		return m, m.spinnerTickCmd()

	case spinnerTickMsg:
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		return m, m.spinnerTickCmd()

	case TaskLogMsg:
		line := sanitizeTerminalText(msg.Line)
		if msg.Model != "" {
			line = "[" + sanitizeTerminalText(msg.Model) + "] " + line
		}
		m.logs[msg.TodoID] = append(m.logs[msg.TodoID], line)
		m.trimTaskLogs(msg.TodoID)
		if m.inDetail && m.detailID != msg.TodoID {
			m.unread[msg.TodoID]++
		}
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
		m.statusText = m.styles.done.Render(fmt.Sprintf("✓ Copied %d lines to clipboard", msg.Lines))

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
		m.statusText = sanitizeTerminalText(msg.Text)

	case ResultMsg:
		m.result = sanitizeTerminalText(msg.Text)

	case TeamInfoMsg:
		m.teamInfo = msg.Info
		m.teamInfo.Decisions = team.RedactedDecisionIndexEntries(m.teamInfo.Decisions)

	case DecisionStateMsg:
		m.teamInfo.Decisions = team.RedactedDecisionIndexEntries(msg.Decisions)

	case OperatorSnapshotMsg:
		m.operatorSummary = operator.BuildSummary(msg.Snapshot)
		m.operatorScope = msg.Snapshot.Scope
		m.operatorSnapshot = msg.Snapshot
		m.hasOperatorSummary = true
		if m.inOperator && m.operatorReady {
			offset := m.operatorVP.YOffset
			m.operatorVP.SetContent(m.buildOperatorContent())
			m.operatorVP.SetYOffset(offset)
		}

	case OperatorDetailsMsg:
		m.operatorEvidence = msg.Evidence
		m.operatorPromotions = slices.Clone(msg.Promotions)
		m.operatorPromotionStatus = msg.PromotionStatus
		m.operatorPromotionTotal = msg.PromotionTotal
		m.operatorPromotionLimited = msg.PromotionLimited
		if m.inOperator && m.operatorReady {
			offset := m.operatorVP.YOffset
			m.operatorVP.SetContent(m.buildOperatorContent())
			m.operatorVP.SetYOffset(offset)
		}

	case SSHSessionsMsg:
		m.teamInfo.SSHSessions = msg.Count

	case TerminalSessionMsg:
		if msg.TodoID != "" && msg.SessionID != "" {
			m.terminals[msg.TodoID] = msg.SessionID
		}

	case terminalAttachFinishedMsg:
		if msg.Err != nil {
			m.statusText = m.styles.errorIcon.Render("✗ terminal attach: " + msg.Err.Error())
		} else {
			m.statusText = m.styles.done.Render("✓ terminal control returned to hufu")
		}

	case FinishedMsg:
		m.finished = true
		m.recentLogs = nil
		m.runResult = msg.Result
		statusStr := team.FormatCanonicalStatus(msg.Result)
		if msg.Result != nil && msg.Result.GoalSatisfied {
			m.statusText = m.styles.doneIcon.Render("✓") + m.styles.dim.Render("  "+statusStr)
		} else if msg.Result != nil && (msg.Result.Outcome == team.RunOutcomeCompleted || msg.Result.Outcome == team.RunOutcomeUnverified) {
			m.statusText = m.styles.pausedIcon.Render("ℹ") + m.styles.dim.Render("  "+statusStr)
		} else if msg.Result != nil && (msg.Result.Outcome == team.RunOutcomeBlocked || msg.Result.Outcome == team.RunOutcomePartial) {
			m.statusText = m.styles.errorIcon.Render("⚠") + m.styles.dim.Render("  "+statusStr)
		} else if msg.Result != nil && (msg.Result.Outcome == team.RunOutcomeFailed || msg.Result.Outcome == team.RunOutcomeCancelled) {
			m.statusText = m.styles.errorIcon.Render("✗") + m.styles.dim.Render("  "+statusStr)
		} else {
			// Unknown outcomes are not evidence of success. Keep the display
			// fail-safe if a newer/invalid outcome reaches this older TUI.
			m.statusText = m.styles.pausedIcon.Render("ℹ") + m.styles.dim.Render("  "+statusStr)
		}
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
		if msg.String() == "T" && m.canSwitchTheme() {
			var theme string
			m, theme = m.cycleTheme()
			m.statusText = "Theme: " + theme
			m.themeGeneration++
			if m.themeMode == ThemeAuto {
				return m, tea.Batch(tea.ClearScreen, m.watchThemeCmd())
			}
			return m, tea.ClearScreen
		}
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
		case OverlayOperator:
			return m.updateOperator(msg)
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
