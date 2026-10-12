package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// ChatActions connects the chat view to the runtime. The view calls these
// only from tea.Cmd functions, never from Update.
type ChatActions struct {
	Submit func(conversation.Command) conversation.Ack
	// Close stops the conversation controller and reports how it ended.
	Close func() conversation.CloseReport
	// Cost renders the cost summary of one run (P3 /cost).
	Cost func(runID string) (string, error)
	// Save exports the conversation to path (P3 /save).
	Save func(path string) error
}

// ChatEventMsg delivers one conversation controller event to the chat view.
type ChatEventMsg struct{ Event conversation.Event }

// ChatCloseRequestMsg closes the session without confirmation, for example
// when the process receives SIGTERM.
type ChatCloseRequestMsg struct{}

// ChatHistoryEntry is one restored conversation message.
type ChatHistoryEntry struct {
	Role    string
	Content string
}

// ChatOptions configures NewChatModel.
type ChatOptions struct {
	Presentation Options
	TeamInfo     TeamInfo
	Actions      ChatActions
	// CommandPrefix keeps command IDs unique within the process.
	CommandPrefix string
	// History is the restored conversation shown before the first turn.
	History []ChatHistoryEntry
	// LastResult is the restored outcome of the previous run, if any.
	LastResult *team.RunResult
}

type chatAckMsg struct {
	cmd conversation.Command
	ack conversation.Ack
	// draft is the composer text the command was built from; it is cleared
	// only if the composer still holds exactly this text.
	draft string
}

type chatClosedMsg struct{ report conversation.CloseReport }

// chatCommandResultMsg carries the output of a slash command that ran in a
// tea.Cmd (for example /cost or /save).
type chatCommandResultMsg struct {
	title string
	body  string
	err   error
}

// Messages an embedded dashboard sends to its chat host instead of quitting.
type (
	embeddedExitMsg    struct{}
	embeddedComposeMsg struct{}
	embeddedCtrlCMsg   struct{}
)

func embeddedMsgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}
