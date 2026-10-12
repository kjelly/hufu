package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/kjelly/hufu/internal/team"
)

// progressCard shows the tasks one turn touched. While the turn runs it is
// rebuilt from the canonical task projection; when the turn settles it is
// frozen so later task updates cannot rewrite it.
type progressCard struct {
	turnID   string
	number   int
	state    string
	frozen   bool
	baseline map[string]team.TaskStatus
	rows     []progressRow
	// rowOffsets is the line of each row within the last render, so a
	// selected row can be scrolled into view.
	rowOffsets []int
}

type progressRow struct {
	id     string
	agent  string
	desc   string
	status team.TaskStatus
	target string
	last   string
}

func newProgressCard(turnID string, number int, tasks []*team.TodoItem) *progressCard {
	baseline := make(map[string]team.TaskStatus, len(tasks))
	for _, task := range tasks {
		if task != nil {
			baseline[task.ID] = task.Status
		}
	}
	return &progressCard{turnID: turnID, number: number, state: "running", baseline: baseline}
}

// refresh rebuilds the rows from the latest projection: only tasks that are
// new since the turn started, or whose status changed, belong to this turn.
func (c *progressCard) refresh(tasks []*team.TodoItem, logs map[string][]string) bool {
	if c == nil || c.frozen {
		return false
	}
	var rows []progressRow
	for _, task := range tasks {
		if task == nil || task.ID == team.CoordTodoID {
			continue
		}
		if status, seen := c.baseline[task.ID]; seen && status == task.Status {
			continue
		}
		row := progressRow{id: task.ID, agent: task.Agent, desc: task.Desc, status: task.Status}
		if !task.ExecutionTarget.IsZero() {
			row.target = task.ExecutionTarget.String()
		}
		if lines := logs[task.ID]; len(lines) > 0 {
			row.last = lines[len(lines)-1]
		}
		rows = append(rows, row)
	}
	if rowsEqual(rows, c.rows) {
		return false
	}
	c.rows = rows
	return true
}

func rowsEqual(a, b []progressRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *progressCard) freeze(state string) {
	if c == nil {
		return
	}
	c.state = state
	c.frozen = true
}

// rowStatus returns the status task id had in this card.
func (c *progressCard) rowStatus(id string) (team.TaskStatus, bool) {
	for _, row := range c.rows {
		if row.id == id {
			return row.status, true
		}
	}
	return "", false
}

// renderProgressCard draws one row per task: agent, status, execution target,
// and task ID; then the task description; then its latest activity.
func renderProgressCard(c *progressCard, selected string, width int, s styleSet) []string {
	if c == nil {
		return nil
	}
	title := fmt.Sprintf("┌ Turn %d · %s", c.number, c.state)
	lines := []string{s.header.Render(truncateCells(title, width))}
	if len(c.rows) == 0 {
		lines = append(lines, s.dim.Render(truncateCells("│ no task activity", width)))
	}
	agentWidth, statusWidth := 0, 0
	for _, row := range c.rows {
		agentWidth = max(agentWidth, len([]rune(row.agent)))
		statusWidth = max(statusWidth, len(string(row.status)))
	}
	agentWidth = min(agentWidth, 18)
	c.rowOffsets = c.rowOffsets[:0]
	for _, row := range c.rows {
		c.rowOffsets = append(c.rowOffsets, len(lines))
		fields := []string{padRight(truncateCells(sanitizeTerminalText(row.agent), agentWidth), agentWidth), padRight(string(row.status), statusWidth)}
		if row.target != "" && width >= 60 {
			fields = append(fields, sanitizeTerminalText(row.target))
		}
		fields = append(fields, "#"+oneLine(row.id))
		marker := "│ "
		if row.id == selected {
			marker = "│›"
		}
		head := truncateCells(marker+strings.Join(fields, "  "), width)
		if row.id == selected {
			lines = append(lines, s.selectedFG.Render(head))
		} else {
			lines = append(lines, progressStatusStyle(row.status, s).Render(head))
		}
		if desc := oneLine(row.desc); desc != "" {
			lines = append(lines, truncateCells("│   "+desc, width))
		}
		if last := oneLine(row.last); last != "" && width >= 60 {
			lines = append(lines, s.dim.Render(truncateCells("│   "+last, width)))
		}
	}
	return append(lines, s.dim.Render(truncateCells("└"+strings.Repeat("─", max(min(width-1, 40), 0)), width)))
}

// oneLine flattens untrusted text to a single sanitized line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(sanitizeTerminalText(text)), " ")
}

func padRight(value string, width int) string {
	if n := len([]rune(value)); n < width {
		return value + strings.Repeat(" ", width-n)
	}
	return value
}

func progressStatusStyle(status team.TaskStatus, s styleSet) lipgloss.Style {
	switch status {
	case team.TaskDone:
		return s.doneIcon
	case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
		return s.errorIcon
	case team.TaskInProgress, team.TaskVerifying:
		return s.progressIcon
	default:
		return s.dim
	}
}
