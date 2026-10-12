package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
)

// chatTickMsg advances the running turn's clock. Time enters the model only
// through this message, so Update stays a pure function.
type chatTickMsg struct {
	turnID string
	at     time.Time
}

// tickInterval refreshes the elapsed time once a second, and rarely on
// e-paper displays where every refresh is costly.
func (m ChatModel) tickInterval() time.Duration {
	if m.dash.displayPreset == DisplayEpaper {
		return 15 * time.Second
	}
	return time.Second
}

func (m ChatModel) tickCmd(turnID string) tea.Cmd {
	return tea.Tick(m.tickInterval(), func(at time.Time) tea.Msg { return chatTickMsg{turnID: turnID, at: at} })
}

// handleTick keeps one tick chain per turn; a tick for a settled or older
// turn ends its chain.
func (m ChatModel) handleTick(msg chatTickMsg) (ChatModel, tea.Cmd) {
	if msg.turnID != m.activeTurn || !m.turnActive() {
		return m, nil
	}
	m.now = msg.at
	return m, m.tickCmd(msg.turnID)
}

func (m ChatModel) turnActive() bool {
	return m.phase == phaseRunning || m.phase == phaseCancelling
}

func (m ChatModel) turnElapsed() time.Duration {
	if m.turnStartedAt.IsZero() || m.now.Before(m.turnStartedAt) {
		return 0
	}
	return m.now.Sub(m.turnStartedAt)
}

// activityText is the latest runtime status line (for example
// "reviewer step 3" or "⟹ bash …") while a turn is running.
func (m ChatModel) activityText() string {
	if !m.turnActive() {
		return ""
	}
	return oneLine(m.dash.statusText)
}

// headerPhase is the phase shown in the header, with the running turn's
// elapsed time.
func (m ChatModel) headerPhase() string {
	if m.turnActive() {
		return m.phaseName() + " " + formatElapsed(m.turnElapsed())
	}
	return m.phaseName()
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
