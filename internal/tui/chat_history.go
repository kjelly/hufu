package tui

import (
	"fmt"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// restoreHistory shows that an existing session was resumed. It only
// displays state; it never starts a turn.
func (m *ChatModel) restoreHistory(history []ChatHistoryEntry, last *team.RunResult) {
	if len(history) == 0 && last == nil {
		return
	}
	m.lastResult = last
	m.transcript.add(chatItem{kind: itemNotice, body: fmt.Sprintf("Resumed session (%d messages)", len(history))})
}

func (m ChatModel) handleControlDone(ev conversation.ControlDone) ChatModel {
	if ev.Err != nil {
		m.transcript.add(chatItem{kind: itemError, body: fmt.Sprintf("/%s failed: %v", controlCommandName(ev.Kind), ev.Err)})
	}
	return m
}

func controlCommandName(kind conversation.CommandKind) string {
	switch kind {
	case conversation.CommandResetContext:
		return "reset"
	case conversation.CommandSwitchTeam:
		return "team"
	default:
		return string(kind)
	}
}

func (m ChatModel) handleCommandResult(msg chatCommandResultMsg) ChatModel {
	if msg.err != nil {
		m.notice = fmt.Sprintf("%s failed: %v", msg.title, msg.err)
		return m
	}
	m.openTextOverlay(msg.title, msg.body)
	return m
}
