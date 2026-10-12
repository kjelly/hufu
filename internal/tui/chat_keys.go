package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
)

// updateKey routes one key press. Priority: ask_user dialog, closing,
// chat overlays, global keys, the tasks view, then the focused pane.
func (m ChatModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.dash.inAskUser {
		// The ask_user dialog owns every key until it is answered.
		return m.forward(msg)
	}
	if m.phase == phaseClosing {
		return m, nil
	}
	if m.overlay != overlayNone {
		return m.updateOverlayKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m.handleCtrlC()
	case "ctrl+t":
		if m.view == viewTasks {
			m.view = viewChat
		} else {
			m.view = viewTasks
		}
		return m, nil
	}
	if m.view == viewTasks {
		return m.forward(msg)
	}
	if m.focus == focusComposer {
		return m.updateComposerKey(msg)
	}
	return m.updateTranscriptKey(msg)
}

func (m ChatModel) updateComposerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.submitComposer()
	case "tab":
		value, matches := completeChatToken(m.composer.Value(), m.dash.teamInfo)
		if value != m.composer.Value() {
			m.composer.SetValue(value)
			m.rerender()
		}
		if len(matches) > 1 {
			m.notice = strings.Join(matches, "  ")
		}
		return m, nil
	case "esc":
		m.focus = focusTranscript
		m.composer.Blur()
		return m, nil
	case "pgup":
		m.transcript.vp.PageUp()
		return m, nil
	case "pgdown":
		m.transcript.vp.PageDown()
		m.transcript.followIfAtBottom()
		return m, nil
	}
	return m.editComposer(msg)
}

// editComposer passes input to the textarea and refreshes the layout,
// which depends on the composer's dynamic height.
func (m ChatModel) editComposer(msg tea.Msg) (ChatModel, tea.Cmd) {
	height := m.composer.Height()
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	if m.composer.Height() != height {
		m.rerender()
	}
	if m.notice != "" && m.submitting == "" && m.phase != phaseCancelling {
		m.notice = composerSizeNotice(m.composer.Value())
	}
	return m, cmd
}

// updatePaste routes bracketed paste to whatever has input focus. A paste
// that would exceed the composer's line cap is refused rather than cut.
func (m ChatModel) updatePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.dash.inAskUser || m.view == viewTasks {
		return m.forward(msg)
	}
	if m.focus != focusComposer || m.overlay != overlayNone || m.phase == phaseClosing {
		return m, nil
	}
	lines := strings.Count(m.composer.Value(), "\n") + strings.Count(msg.Content, "\n") + 1
	if lines > maxComposerLines {
		m.notice = "paste refused: the message would exceed 10000 lines"
		return m, nil
	}
	return m.editComposer(msg)
}

func (m ChatModel) updateTranscriptKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "g", "home":
		m.transcript.vp.GotoTop()
		return m, nil
	case "G", "end":
		m.transcript.vp.GotoBottom()
		m.transcript.followIfAtBottom()
		return m, nil
	case "i", "a", "enter":
		return m.focusComposer()
	case "T":
		return m.cycleTheme()
	case "?":
		m.openTextOverlay("Help", m.helpText())
		return m, nil
	}
	// Scrolling keys (j/k, arrows, PgUp/PgDn, ctrl+u/d, …) use the
	// viewport's own key map.
	var cmd tea.Cmd
	m.transcript.vp, cmd = m.transcript.vp.Update(msg)
	m.transcript.followIfAtBottom()
	return m, cmd
}

func (m ChatModel) updateOverlayKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.overlay == overlayConfirmExit {
		switch key {
		case "y", "Y", "enter", "ctrl+c":
			return m.startClose()
		case "n", "N", "esc", "q":
			m.overlay = overlayNone
		}
		return m, nil
	}
	switch key {
	case "esc", "q", "enter", "ctrl+c":
		m.overlay = overlayNone
	case "j", "down":
		m.overlayLines++
	case "k", "up":
		m.overlayLines = max(m.overlayLines-1, 0)
	case "g":
		m.overlayLines = 0
	}
	return m, nil
}

// handleCtrlC applies the layered Ctrl+C contract: cancel the running turn,
// then offer to quit; when idle, clear the draft, then offer to quit.
func (m ChatModel) handleCtrlC() (ChatModel, tea.Cmd) {
	switch {
	case m.phase == phaseClosing:
		return m, nil
	case m.phase == phaseRunning:
		return m.sendControl(conversation.CommandCancelTurn, "")
	case m.phase == phaseCancelling:
		m.overlay = overlayConfirmExit
		return m, nil
	case m.view == viewChat && strings.TrimSpace(m.composer.Value()) != "":
		m.composer.Reset()
		m.rerender()
		m.notice = "draft cleared — Ctrl+C again to quit"
		return m, nil
	default:
		m.overlay = overlayConfirmExit
		return m, nil
	}
}
