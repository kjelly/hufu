package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/team"
)

// updateMouse handles mouse input. In the detail view a left drag selects log
// lines (copied on release); on the dashboard a left click opens a card and
// the wheel moves the cursor.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	_, click := msg.(tea.MouseClickMsg)
	_, motion := msg.(tea.MouseMotionMsg)
	_, release := msg.(tea.MouseReleaseMsg)
	left := mouse.Button == tea.MouseLeft
	if m.inDetail {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		if logIndex, ok := m.detailLogIndexAt(mouse.Y); ok {
			switch {
			case click && left:
				m.inVisual = true
				m.cursorLine, m.visualStart, m.visualEnd = logIndex, logIndex, logIndex
				m.vp.SetContent(m.buildDetailContent())
			case motion && left && m.inVisual:
				m.cursorLine, m.visualEnd = logIndex, logIndex
				m.vp.SetContent(m.buildDetailContent())
			}
		}
		if release && m.inVisual && m.visualStart != m.visualEnd {
			text := m.getVisualSelection()
			m.inVisual = false
			m.visualStart, m.visualEnd = 0, 0
			if m.vpReady {
				m.vp.SetContent(m.buildDetailContent())
			}
			if text != "" {
				return m, copyToClipboard(text)
			}
		}
		return m, cmd
	}
	if !m.mouseEnabled {
		return m, nil
	}
	if click && left {
		item, col, row := m.dashboardItemAt(mouse.X, mouse.Y)
		if item == nil {
			return m, nil
		}
		m.col, m.row = col, row
		m.detailID = item.ID
		m.expandedLogIndex = -1
		delete(m.unread, item.ID)
		m.inDetail = true
		m.cursorLine = max(len(m.logs[item.ID])-1, 0)
		if m.vpReady {
			m.vp.SetContent(m.buildDetailContent())
			m.vp.GotoBottom()
		}
		return m, nil
	}
	if _, wheel := msg.(tea.MouseWheelMsg); wheel {
		switch mouse.Button {
		case tea.MouseWheelUp:
			if m.row > 0 {
				m.row--
				m.scrollCursorIntoView()
			}
		case tea.MouseWheelDown:
			if m.row < len(m.colItems(m.col))-1 {
				m.row++
				m.scrollCursorIntoView()
			}
		}
	}
	return m, nil
}

// detailLogIndexAt maps a screen row in the detail view to a log index.
func (m Model) detailLogIndexAt(y int) (int, bool) {
	item := m.findTask(m.detailID)
	if item == nil {
		return 0, false
	}
	clickY := y - len(strings.Split(m.renderDetailHeader(item), "\n"))
	lines := m.logs[m.detailID]
	if clickY < 0 || clickY >= m.vp.Height() || len(lines) == 0 {
		return 0, false
	}
	logIndex := m.mapRenderedLineToLogIndex(m.vp.YOffset()+clickY, max(m.vp.Width(), 20))
	return logIndex, logIndex >= 0 && logIndex < len(lines)
}

// dashboardItemAt maps screen coordinates to the same card order rendered by
// renderCol and renderCompactCol. Divider and spacer cells are not cards.
func (m Model) dashboardItemAt(x, y int) (*team.TodoItem, int, int) {
	if m.width < 60 || x < 0 || x >= m.width {
		return nil, 0, 0
	}
	layout := m.dashboardLayout()
	cardY := y - layout.bodyY - 2 // column title and its blank line
	if cardY < 0 || cardY >= layout.bodyH-2 {
		return nil, 0, 0
	}
	columns := 6
	if m.isCompact() {
		columns = 3
	}
	colW := (m.width - (columns - 1)) / columns
	visualCol := -1
	for col, start := 0, 0; col < columns; col++ {
		end := start + colW
		if x >= start && x < end {
			visualCol = col
			break
		}
		start = end + 1
	}
	if visualCol < 0 {
		return nil, 0, 0
	}
	if m.isCompact() {
		return m.compactItemAt(visualCol, cardY, colW)
	}
	items := m.colItems(visualCol)
	return m.itemAt(items, m.scrollOff[visualCol], cardY, colW, visualCol)
}

func (m Model) compactItemAt(group, cardY, width int) (*team.TodoItem, int, int) {
	items := m.compactItems(group)
	item, _, idx := m.itemAt(items, m.compactScrollOff[group], cardY, width, group)
	if item == nil {
		return nil, 0, 0
	}
	for _, col := range compactGroupColumns[group] {
		count := len(m.colItems(col))
		if idx < count {
			return item, col, idx
		}
		idx -= count
	}
	return nil, 0, 0
}

func (m Model) itemAt(items []*team.TodoItem, start, cardY, width, col int) (*team.TodoItem, int, int) {
	line := 0
	for i := max(start, 0); i < len(items); i++ {
		height := len(m.itemLines(items[i], false, false, width))
		if cardY >= line && cardY < line+height {
			return items[i], col, i
		}
		line += height + 1 // blank line between cards
	}
	return nil, 0, 0
}
