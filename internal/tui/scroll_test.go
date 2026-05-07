package tui

import (
	"fmt"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
)

func makeTasks(n int, status team.TaskStatus) []*team.TodoItem {
	items := make([]*team.TodoItem, n)
	for i := range items {
		items[i] = &team.TodoItem{
			ID:     fmt.Sprintf("%d", i+1),
			Agent:  "agent",
			Desc:   "task",
			Status: status,
		}
	}
	return items
}

func testModel(items []*team.TodoItem, height int) Model {
	m := Model{
		tasks:   items,
		height:  height,
		width:   80,
		col:     0,
		row:     0,
		scrollOff: [5]int{},
	}
	return m
}

func TestScrollCursorIntoView_SelectedBelow(t *testing.T) {
	items := makeTasks(20, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 15
	m.scrollOff[0] = 0

	m.scrollCursorIntoView()

	if m.scrollOff[0] == 0 {
		t.Errorf("expected scroll offset > 0 when cursor is at row 15, got %d", m.scrollOff[0])
	}
	if m.scrollOff[0] >= m.row {
		t.Errorf("scroll offset %d should be <= selected row %d", m.scrollOff[0], m.row)
	}
}

func TestScrollCursorIntoView_SelectedAbove(t *testing.T) {
	items := makeTasks(20, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 2
	m.scrollOff[0] = 10

	m.scrollCursorIntoView()

	if m.scrollOff[0] > m.row {
		t.Errorf("scroll offset %d should be <= selected row %d when cursor is above viewport", m.scrollOff[0], m.row)
	}
	if m.scrollOff[0] < 0 {
		t.Errorf("scroll offset should not be negative, got %d", m.scrollOff[0])
	}
}

func TestScrollCursorIntoView_SelectedInView(t *testing.T) {
	items := makeTasks(5, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 2
	m.scrollOff[0] = 0

	offBefore := m.scrollOff[0]
	m.scrollCursorIntoView()

	if m.scrollOff[0] != offBefore {
		t.Errorf("scroll offset changed from %d to %d when selected item was already in view", offBefore, m.scrollOff[0])
	}
}

func TestScrollCursorIntoView_EmptyColumn(t *testing.T) {
	m := testModel(nil, 30)
	m.col = 0
	m.row = 0

	m.scrollCursorIntoView()

	if m.scrollOff[0] != 0 {
		t.Errorf("expected scroll offset 0 for empty column, got %d", m.scrollOff[0])
	}
}

func TestScrollCursorIntoView_JumpToBottom(t *testing.T) {
	items := makeTasks(50, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 49
	m.scrollOff[0] = 0

	m.scrollCursorIntoView()

	if m.scrollOff[0] >= m.row {
		t.Errorf("scroll offset %d should be < selected row %d", m.scrollOff[0], m.row)
	}

	bodyH := m.colBodyHeight()
	availableLines := bodyH - 2
	if availableLines <= 0 {
		availableLines = 1
	}

	col := m.colItems(m.col)
	lineCount := 2
	found := false
	for i := m.scrollOff[0]; i < len(col); i++ {
		il := len(m.itemLines(col[i], false, false, 0))
		if il == 0 {
			il = 2
		}
		if i == m.row {
			found = true
			break
		}
		lineCount += il
		if i < len(col)-1 {
			lineCount++
		}
	}
	if !found {
		t.Errorf("selected row %d not found in column starting from scrollOff %d", m.row, m.scrollOff[0])
	}
	if lineCount > bodyH {
		t.Errorf("selected item starts at line %d which exceeds body height %d", lineCount, bodyH)
	}
}

func TestScrollCursorIntoView_RowClamp(t *testing.T) {
	items := makeTasks(5, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 100

	m.scrollCursorIntoView()

	if m.row >= len(items) {
		t.Errorf("row %d should be clamped to < %d", m.row, len(items))
	}
}

func TestScrollCursorIntoView_SingleItem(t *testing.T) {
	items := makeTasks(1, team.TaskPending)
	m := testModel(items, 30)
	m.col = 0
	m.row = 0
	m.scrollOff[0] = 0

	m.scrollCursorIntoView()

	if m.scrollOff[0] != 0 {
		t.Errorf("expected scroll offset 0 for single item, got %d", m.scrollOff[0])
	}
}