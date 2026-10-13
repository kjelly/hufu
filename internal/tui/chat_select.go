package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/team"
)

// cardSelection identifies one task row inside one progress card.
type cardSelection struct {
	item int    // transcript index of the progress card
	task string // task ID of the row
}

func (m ChatModel) selectableRows() []cardSelection {
	var rows []cardSelection
	for i, item := range m.transcript.items {
		if item.kind != itemProgress || item.card == nil {
			continue
		}
		for _, row := range item.card.rows {
			rows = append(rows, cardSelection{item: i, task: row.id})
		}
	}
	return rows
}

// moveSelection moves the progress-row selection by delta rows; the first
// move selects the newest row.
func (m ChatModel) moveSelection(delta int) ChatModel {
	rows := m.selectableRows()
	if len(rows) == 0 {
		m.notice = "no task rows to select yet"
		return m
	}
	next := len(rows) - 1
	if m.selection != nil {
		for i, row := range rows {
			if row == *m.selection {
				next = min(max(i+delta, 0), len(rows)-1)
				break
			}
		}
	}
	m = m.setSelection(&rows[next])
	m.rerender()
	m.revealSelection()
	return m
}

func (m ChatModel) setSelection(selection *cardSelection) ChatModel {
	for _, sel := range []*cardSelection{m.selection, selection} {
		if sel != nil && sel.item < len(m.transcript.items) {
			m.transcript.items[sel.item].version++
		}
	}
	m.selection = selection
	for i := range m.transcript.items {
		m.transcript.items[i].selectedTask = ""
	}
	if selection != nil {
		m.transcript.items[selection.item].selectedTask = selection.task
	}
	return m
}

// revealSelection scrolls the viewport so the selected row is visible.
func (m *ChatModel) revealSelection() {
	if m.selection == nil {
		return
	}
	line := 0
	for i := 0; i < m.selection.item; i++ {
		line += len(m.transcript.items[i].lines)
	}
	card := m.transcript.items[m.selection.item].card
	for i, row := range card.rows {
		if row.id == m.selection.task && i < len(card.rowOffsets) {
			line += card.rowOffsets[i]
			break
		}
	}
	m.transcript.vp.EnsureVisible(line, 0, 0)
	m.transcript.followIfAtBottom()
}

// openSelection opens the selected task's detail in the tasks view. A
// frozen card may name a task that later turns changed; that is reported
// instead of being hidden.
func (m ChatModel) openSelection() (ChatModel, tea.Cmd) {
	sel := m.selection
	card := m.transcript.items[sel.item].card
	task := m.dash.findTask(sel.task)
	if task == nil {
		m.notice = fmt.Sprintf("task %s is no longer in the task list", sel.task)
		return m, nil
	}
	if frozen, ok := card.rowStatus(sel.task); ok && card.frozen && frozen != task.Status {
		m.notice = fmt.Sprintf("task %s has newer activity since turn %d (%s → %s)", sel.task, card.number, frozen, task.Status)
	}
	m.dash = m.dash.openTaskDetail(task)
	m.view = viewTasks
	return m, nil
}

// openTaskDetail opens the detail view for item, scrolled to its newest log.
func (m Model) openTaskDetail(item *team.TodoItem) Model {
	m.detailID = item.ID
	m.expandedLogIndex = -1
	delete(m.unread, item.ID)
	m.inDetail = true
	m.cursorLine = max(len(m.logs[item.ID])-1, 0)
	if m.vpReady {
		m.vp.SetContent(m.buildDetailContent())
		m.vp.GotoBottom()
	}
	return m
}
