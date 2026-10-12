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
}

type progressRow struct {
	id     string
	agent  string
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
		row := progressRow{id: task.ID, agent: task.Agent, status: task.Status}
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

func renderProgressCard(c *progressCard, width int, s styleSet) []string {
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
	for _, row := range c.rows {
		fields := []string{padRight(truncateCells(sanitizeTerminalText(row.agent), agentWidth), agentWidth), padRight(string(row.status), statusWidth)}
		if row.target != "" && width >= 60 {
			fields = append(fields, sanitizeTerminalText(row.target))
		}
		if row.last != "" && width >= 60 {
			fields = append(fields, strings.ReplaceAll(row.last, "\n", " "))
		}
		line := "│ " + strings.Join(fields, "  ")
		lines = append(lines, progressStatusStyle(row.status, s).Render(truncateCells(line, width)))
	}
	return append(lines, s.dim.Render(truncateCells("└"+strings.Repeat("─", max(min(width-1, 40), 0)), width)))
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
