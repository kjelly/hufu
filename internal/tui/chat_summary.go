package tui

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// maxSummaryFailures caps how many failed tasks the turn summary names.
const maxSummaryFailures = 3

// summaryStatusOrder fixes the order of status counts in the summary.
var summaryStatusOrder = []team.TaskStatus{
	team.TaskDone, team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete,
	team.TaskSkipped, team.TaskInProgress, team.TaskVerifying, team.TaskPaused,
	team.TaskPlanned, team.TaskPending,
}

func isFailedTaskStatus(status team.TaskStatus) bool {
	return status == team.TaskError || status == team.TaskBlocked || status == team.TaskProtocolIncomplete
}

// turnSummary is the one-line digest appended after a turn settles, so the
// outcome and any failed task stay visible at the bottom even when a long
// answer pushes the progress card out of view. It reports whether a task
// failed.
func turnSummary(card *progressCard, outcome string) (string, bool) {
	if card == nil || len(card.rows) == 0 {
		return "", false
	}
	counts := make(map[team.TaskStatus]int)
	var failures []string
	for _, row := range card.rows {
		counts[row.status]++
		if isFailedTaskStatus(row.status) {
			failures = append(failures, describeFailedRow(row))
		}
	}
	parts := []string{fmt.Sprintf("Turn %d · %s", card.number, outcome)}
	for _, status := range summaryStatusOrder {
		if n := counts[status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	line := strings.Join(parts, " · ")
	if len(failures) == 0 {
		return line, false
	}
	named := failures[:min(len(failures), maxSummaryFailures)]
	line += " — failed: " + strings.Join(named, ", ")
	if extra := len(failures) - len(named); extra > 0 {
		line += fmt.Sprintf(", +%d more", extra)
	}
	return line, true
}

// describeFailedRow names a failed task by ID, then its contract (or a short
// description), then its agent.
func describeFailedRow(row progressRow) string {
	label := "#" + oneLine(row.id)
	what := oneLine(row.contract)
	if what == "" {
		what = oneLine(row.desc)
		if runes := []rune(what); len(runes) > 48 {
			what = string(runes[:47]) + "…"
		}
	}
	if what != "" {
		label += " " + what
	}
	if agent := oneLine(row.agent); agent != "" {
		label += " (" + agent + ")"
	}
	return label
}
