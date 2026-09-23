package tui

import "github.com/kjelly/hufu/internal/team"

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
