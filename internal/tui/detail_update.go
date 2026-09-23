package tui

import (
	"os/exec"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

//nolint:gocyclo // Keep the detail keymap in one switch so navigation precedence stays visible.
func (m Model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.copyNotice = ""
	if m.detailSearchActive {
		return m.updateDetailSearch(msg)
	}
	if m.inVisual {
		return m.updateVisual(msg)
	}
	switch msg.String() {
	case "esc", "backspace":
		m.inDetail = false
		m.detailSearchQuery = ""
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
	case "t":
		return m.attachTerminal()
	case "o":
		if _, ok := m.fullLogs[m.detailID][m.cursorLine]; ok {
			if m.expandedLogIndex == m.cursorLine {
				m.expandedLogIndex = -1
			} else {
				m.expandedLogIndex = m.cursorLine
			}
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
				m.followCursor()
			}
		}
		return m, nil
	case "/":
		m.detailSearchActive = true
		m.detailSearchInput.SetValue(m.detailSearchQuery)
		m.detailSearchInput.Focus()
		return m, m.detailSearchInput.Cursor.BlinkCmd()
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
		return m.jumpToDetailMatch(1)
	case "N":
		return m.jumpToDetailMatch(-1)
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

func (m Model) updateDetailSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.detailSearchQuery = strings.TrimSpace(m.detailSearchInput.Value())
		m.detailSearchActive = false
		m.detailSearchInput.Blur()
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
		}
		if m.detailSearchQuery != "" {
			return m.jumpToDetailMatch(1)
		}
		return m, nil
	case "esc":
		m.detailSearchActive = false
		m.detailSearchInput.Blur()
		return m, nil
	case "ctrl+c":
		m.detailSearchActive = false
		m.detailSearchInput.Blur()
		return m.handleCtrlC()
	}
	var cmd tea.Cmd
	m.detailSearchInput, cmd = m.detailSearchInput.Update(msg)
	return m, cmd
}

func (m Model) jumpToDetailMatch(direction int) (tea.Model, tea.Cmd) {
	if m.detailSearchQuery == "" {
		return m, nil
	}
	lines := m.logs[m.detailID]
	if len(lines) == 0 {
		return m, nil
	}
	query := strings.ToLower(m.detailSearchQuery)
	for step := 1; step <= len(lines); step++ {
		idx := (m.cursorLine + direction*step + len(lines)*2) % len(lines)
		if strings.Contains(strings.ToLower(lines[idx]), query) {
			m.cursorLine = idx
			m.followCursor()
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
			return m, nil
		}
	}
	m.statusText = "No matches in task log"
	return m, nil
}

func (m Model) attachTerminal() (tea.Model, tea.Cmd) {
	if m.teamInfo.Workspace == "" || m.teamInfo.HufuBinary == "" {
		m.statusText = m.styles.errorIcon.Render("terminal attach is unavailable without workspace and hufu binary")
		return m, nil
	}
	sessionID := m.terminals[m.detailID]
	if sessionID == "" {
		m.statusText = m.styles.dim.Render("no PTY terminal session is associated with this task")
		return m, nil
	}
	cmd := exec.Command(m.teamInfo.HufuBinary, "terminal", "attach", sessionID, "--workspace", m.teamInfo.Workspace)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return terminalAttachFinishedMsg{Err: err} })
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
