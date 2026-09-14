package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kjelly/hufu/internal/team"
)

// updateColumns is the centralized dashboard keymap; each branch maps one
// documented key journey and intentionally remains visible in one switch.
//
//nolint:gocyclo // Splitting the keymap would obscure conflicts and precedence.
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
		if !m.owner {
			return m, tea.Quit
		}
		m.inConfirm = true
		m.confirmChoice = 0
		return m, nil
	case "q":
		if m.finished || !m.owner {
			return m, tea.Quit
		}
	case "r":
		return m.handleReportKey()
	case "c":
		if !m.finished || m.IsChat {
			m.inPromptInput = true
			m.promptInput.SetValue("")
			m.promptInput.Focus()
			return m, m.blinkCmd()
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
		return m, m.blinkCmd()
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
			delete(m.unread, m.detailID)
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
	case "L":
		m.inOperator = true
		m.loadOperatorContent()
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
			m.statusText = m.styles.dim.Render("Generating report...")
		default:
		}
	}
	return m, nil
}

func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if !m.owner {
		return m, tea.Quit
	}
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
	m.statusText = m.styles.wrapUp.Render("⏹ Finishing active tasks — Ctrl+C to force quit")
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
	header := m.styles.header.Render("─── ACTIVITY LOG ──────────────────────────────") + " " +
		m.styles.dim.Render("[j/k] scroll [esc/q/a] close")
	b.WriteString(header)
	b.WriteString("\n\n")

	// Initialize viewport if needed (defensive: should already be init from Update)
	if !m.vpReady {
		// Viewport not ready - show placeholder
		b.WriteString(m.styles.dim.Render("Activity log initializing..."))
		b.WriteString("\n")
		footer := m.styles.dim.Render("──────────────────────────────────────────────────────────────────────────────")
		b.WriteString(footer)
		return b.String()
	}

	// Update viewport content
	m.vp.SetContent(m.formatActivityLogContent())

	// Viewport content
	b.WriteString(m.vp.View())
	b.WriteString("\n")

	// Footer
	footer := m.styles.dim.Render("──────────────────────────────────────────────────────────────────────────────")
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
		return m.styles.dim.Render("No activity log entries yet.")
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
