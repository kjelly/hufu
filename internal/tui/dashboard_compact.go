package tui

import "github.com/kjelly/hufu/internal/team"

var compactGroupColumns = [3][]int{{0, 1}, {2}, {3, 4, 5}}

func compactGroupForCol(col int) int {
	switch {
	case col <= 1:
		return 0
	case col == 2:
		return 1
	default:
		return 2
	}
}

func (m Model) compactItems(group int) []*team.TodoItem {
	var items []*team.TodoItem
	for _, col := range compactGroupColumns[group] {
		items = append(items, m.colItems(col)...)
	}
	return items
}

func (m Model) compactSelectedIndex(group int) int {
	idx := m.row
	for _, col := range compactGroupColumns[group] {
		if col == m.col {
			return idx
		}
		idx += len(m.colItems(col))
	}
	return 0
}

func (m *Model) selectCompactItem(group, index int) {
	items := m.compactItems(group)
	if len(items) == 0 {
		m.col = compactGroupColumns[group][0]
		m.row = 0
		return
	}
	index = max(0, min(index, len(items)-1))
	for _, col := range compactGroupColumns[group] {
		count := len(m.colItems(col))
		if index < count {
			m.col, m.row = col, index
			m.scrollCompactCursorIntoView()
			return
		}
		index -= count
	}
}

func (m *Model) scrollCompactCursorIntoView() {
	group := compactGroupForCol(m.col)
	items := m.compactItems(group)
	if len(items) == 0 {
		m.compactScrollOff[group] = 0
		return
	}
	selected := min(m.compactSelectedIndex(group), len(items)-1)
	off := &m.compactScrollOff[group]
	if *off > selected {
		*off = selected
	}
	width := (m.width - 2) / 3
	available := max(m.colBodyHeight()-2, 1)
	for {
		used := 0
		for i := *off; i <= selected; i++ {
			used += len(m.itemLines(items[i], false, false, width))
			if i < selected {
				used++
			}
		}
		if used <= available || *off >= selected {
			return
		}
		*off++
	}
}
