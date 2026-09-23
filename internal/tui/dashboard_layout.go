package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

type dashboardLayout struct {
	summary, prompt, progress, status, feed string
	feedH, bodyH, bodyY                     int
}

func (m Model) isDashboardCollapsed() bool {
	if m.dashboardHeaderToggled {
		return m.dashboardCollapsed
	}
	return m.height < 32
}

func (m Model) dashboardLayout() dashboardLayout {
	l := dashboardLayout{progress: m.renderProgressBar(m.width), status: m.renderStatusArea(m.width)}
	if m.isDashboardCollapsed() {
		summary := m.liveSummary()
		if m.hasOperatorSummary {
			summary = m.operatorSummary
		}
		l.summary = fmt.Sprintf("State: %s · What: %s · Next: %s",
			truncateCells(summary.State, 12), truncateCells(summary.What, max((m.width-40)/2, 4)),
			truncateCells(summary.Next, max((m.width-40)/2, 4)))
		l.prompt = "Task: " + truncateCells(m.prompt, max(m.width-6, 1))
	} else {
		l.summary = m.renderSummaryStrip(m.width)
		l.prompt = m.renderPromptWidget(m.width)
		l.feed = m.renderActivityFeed(m.width)
		l.feedH = m.countFeedLines()
	}
	feedTotal := 0
	if l.feedH > 0 {
		feedTotal = l.feedH + 1
	}
	progressH := 0
	if l.progress != "" {
		progressH = 1
	}
	l.bodyY = lipgloss.Height(l.summary) + 1 + lipgloss.Height(l.prompt) + 1 + progressH + m.statusAreaHeight() + 1 + feedTotal
	l.bodyH = max(m.height-l.bodyY-2, 2)
	return l
}
