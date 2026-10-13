package tui

import (
	"fmt"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// restoreHistory shows the resumed session's conversation once, marked as
// restored. It only displays state; opening the chat never starts a turn or
// any recovery.
func (m *ChatModel) restoreHistory(history []ChatHistoryEntry, last *team.RunResult) {
	for _, entry := range history {
		switch entry.Role {
		case "user":
			m.transcript.add(chatItem{kind: itemUser, body: entry.Content, restored: true})
		case "assistant":
			m.transcript.add(chatItem{kind: itemAssistant, title: "Coordinator", body: entry.Content, restored: true})
		default:
			m.transcript.add(chatItem{kind: itemNotice, body: entry.Role + ": " + entry.Content, restored: true})
		}
	}
	if last == nil {
		return
	}
	m.lastResult = last
	m.transcript.add(chatItem{kind: itemNotice, body: "last run: " + team.FormatCanonicalStatus(last), restored: true})
	if last.Continuation != nil || len(last.UnresolvedTasks) > 0 {
		m.transcript.add(chatItem{kind: itemNotice, restored: true,
			body: "previous run has unresolved work — run 'hufu session status' or 'hufu resume' outside chat"})
	}
}

func (m ChatModel) handleControlDone(ev conversation.ControlDone) ChatModel {
	if ev.Err != nil {
		m.transcript.add(chatItem{kind: itemError, body: fmt.Sprintf("/%s failed: %v", controlCommandName(ev.Kind), ev.Err)})
		return m
	}
	switch ev.Kind {
	case conversation.CommandResetContext:
		m.transcript.add(chatItem{kind: itemNotice, body: "Conversation context cleared."})
	case conversation.CommandSwitchTeam:
		m.transcript.add(chatItem{kind: itemNotice, body: fmt.Sprintf("Switched to team %s. Conversation context cleared.", ev.Target)})
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
	switch {
	case msg.err != nil:
		m.notice = fmt.Sprintf("%s failed: %v", msg.title, msg.err)
	case msg.body == "":
		m.notice = msg.title
	default:
		m.openTextOverlay(msg.title, msg.body)
	}
	return m
}
