package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

type chatFocus int

const (
	focusComposer chatFocus = iota
	focusTranscript
)

type chatViewMode int

const (
	viewChat chatViewMode = iota
	viewTasks
)

type chatPhase int

const (
	phaseIdle chatPhase = iota
	phaseRunning
	phaseCancelling
	phaseClosing
)

type chatOverlay int

const (
	overlayNone chatOverlay = iota
	overlayText
	overlayConfirmExit
	overlayPalette
)

// ChatModel is the chat-first view. It renders the conversation, owns the
// composer, and hosts the task dashboard as a secondary view. Turn state is
// driven only by controller events; task state only by the canonical task
// projection the embedded dashboard receives.
type ChatModel struct {
	dash     Model
	actions  ChatActions
	prefix   string
	seq      int
	composer textarea.Model
	focus    chatFocus
	view     chatViewMode
	phase    chatPhase
	overlay  chatOverlay

	overlayTitle string
	overlayBody  string
	overlayLines int // scroll offset inside a text overlay

	transcript    chatTranscript
	activeTurn    string
	turnCount     int
	queued        int
	submitting    string // ID of the prompt command awaiting its Ack
	notice        string
	lastResult    *team.RunResult
	lastTurnID    string
	lastRunIDs    []string
	selection     *cardSelection
	turnStartedAt time.Time
	now           time.Time
	palette       paletteState
	closeReport   *conversation.CloseReport

	width  int
	height int
}

// NewChatModel builds the chat view with an embedded task dashboard.
func NewChatModel(opts ChatOptions) ChatModel {
	presentation := opts.Presentation
	presentation.Owner = true
	if presentation.DisplayPreset == DisplayEpaper {
		presentation.Spinner = false
	}
	dash := NewWithOptions("", opts.TeamInfo, presentation)
	dash.embedded = true
	dash.IsChat = false
	dash.inPromptInput = false

	m := ChatModel{
		dash:       dash,
		actions:    opts.Actions,
		prefix:     opts.CommandPrefix,
		composer:   newChatComposer(dash.displayPreset, 6),
		transcript: newChatTranscript(),
	}
	if m.prefix == "" {
		m.prefix = "chat"
	}
	applyComposerStyles(&m.composer, dash.styles)
	m.composer.Focus()
	m.restoreHistory(opts.History, opts.LastResult)
	return m
}

// Init starts the theme watcher and, outside e-paper mode, cursor blinking.
func (m ChatModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.dash.Init()}
	if m.dash.displayPreset != DisplayEpaper {
		cmds = append(cmds, textarea.Blink)
	}
	return tea.Batch(cmds...)
}

// CloseReport returns the controller close report once the session closed.
func (m ChatModel) CloseReport() *conversation.CloseReport { return m.closeReport }

// Update dispatches one message. It is pure: runtime calls happen only in
// the returned commands.
func (m ChatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		var cmd tea.Cmd
		m, cmd = m.forward(msg)
		m.layoutComposer()
		m.rerender()
		return m, cmd
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case tea.PasteMsg:
		return m.updatePaste(msg)
	case ChatEventMsg:
		m = m.handleEvent(msg.Event)
		m.rerender()
		if started, ok := msg.Event.(conversation.TurnStarted); ok {
			return m, m.tickCmd(started.TurnID)
		}
		return m, nil
	case chatTickMsg:
		return m.handleTick(msg)
	case chatAckMsg:
		m = m.handleAck(msg)
		m.rerender()
		return m, nil
	case chatCommandResultMsg:
		m = m.handleCommandResult(msg)
		m.rerender()
		return m, nil
	case chatClosedMsg:
		report := msg.report
		m.closeReport = &report
		return m, tea.Quit
	case ChatCloseRequestMsg:
		return m.startClose()
	case embeddedExitMsg:
		m.view = viewChat
		return m, nil
	case embeddedComposeMsg:
		m.view = viewChat
		return m.focusComposer()
	case embeddedCtrlCMsg:
		return m.handleCtrlC()
	case AskUserMsg:
		// A question for the user always surfaces in the chat view.
		m.view = viewChat
		return m.forward(msg)
	case ChatNoticeMsg:
		m.transcript.add(chatItem{kind: itemNotice, body: msg.Text})
		m.rerender()
		return m, nil
	case FinishedMsg, ResultMsg:
		// The dashboard's session-end messages do not apply to chat turns.
		return m, nil
	case TasksUpdatedMsg, TaskLogMsg:
		var cmd tea.Cmd
		m, cmd = m.forward(msg)
		m.refreshLiveCard()
		m.rerender()
		return m, cmd
	}
	var cmds []tea.Cmd
	var cmd tea.Cmd
	theme := m.themeKey()
	m, cmd = m.forward(msg)
	cmds = append(cmds, cmd)
	if m.themeKey() != theme {
		applyComposerStyles(&m.composer, m.dash.styles)
		m.rerender()
	}
	m.composer, cmd = m.composer.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m ChatModel) forward(msg tea.Msg) (ChatModel, tea.Cmd) {
	next, cmd := m.dash.Update(msg)
	m.dash = next.(Model)
	return m, cmd
}

func (m ChatModel) themeKey() string {
	return fmt.Sprintf("%s/%s/%d/%t", m.dash.themeMode, m.dash.effectiveTheme, m.dash.themeGeneration, m.dash.noColor)
}

func (m *ChatModel) rerender() {
	m.transcript.rerender(m.transcriptWidth(), m.transcriptHeight(), m.themeKey(), m.dash.styles)
}

func (m ChatModel) nextCommandID() (ChatModel, string) {
	m.seq++
	return m, fmt.Sprintf("%s-%d", m.prefix, m.seq)
}

func (m ChatModel) submitCmd(cmd conversation.Command, draft string) tea.Cmd {
	submit := m.actions.Submit
	return func() tea.Msg {
		if submit == nil {
			return chatAckMsg{cmd: cmd, draft: draft, ack: conversation.Ack{CommandID: cmd.ID, Status: conversation.AckRejected, Reason: "chat runtime unavailable"}}
		}
		return chatAckMsg{cmd: cmd, draft: draft, ack: submit(cmd)}
	}
}

// submitComposer sends the composer content as a prompt or runs it as a
// slash command. The draft stays in the composer until the controller
// admits it.
func (m ChatModel) submitComposer() (ChatModel, tea.Cmd) {
	if m.submitting != "" {
		return m, nil
	}
	draft := m.composer.Value()
	text := strings.TrimSpace(draft)
	if text == "" {
		return m, nil
	}
	in := parseChatInput(text)
	switch in.kind {
	case inputUnknownCommand:
		m.notice = fmt.Sprintf("unknown command /%s — use //%s to send literally", in.name, in.name)
		return m, nil
	case inputCommand:
		return m.runChatCommand(in, draft)
	}
	if len(in.text) > conversation.MaxPromptBytes {
		m.notice = fmt.Sprintf("message is %d KiB; the limit is %d KiB", len(in.text)/1024, conversation.MaxPromptBytes/1024)
		return m, nil
	}
	var id string
	m, id = m.nextCommandID()
	cmd := conversation.Command{ID: id, Kind: conversation.CommandSubmitPrompt, Text: in.text}
	m.submitting = id
	m.notice = "sending…"
	return m, m.submitCmd(cmd, draft)
}

func (m ChatModel) sendControl(kind conversation.CommandKind, target string) (ChatModel, tea.Cmd) {
	var id string
	m, id = m.nextCommandID()
	return m, m.submitCmd(conversation.Command{ID: id, Kind: kind, Target: target}, "")
}

func (m ChatModel) clearComposer(draft string) ChatModel {
	if draft != "" && m.composer.Value() == draft {
		m.composer.Reset()
		m.layoutComposer()
	}
	return m
}

func (m ChatModel) handleAck(msg chatAckMsg) ChatModel {
	ack := msg.ack
	switch msg.cmd.Kind {
	case conversation.CommandSubmitPrompt:
		if m.submitting == msg.cmd.ID {
			m.submitting = ""
		}
		switch ack.Status {
		case conversation.AckAccepted, conversation.AckQueued:
			m = m.clearComposer(msg.draft)
			m.notice = ""
			item := chatItem{kind: itemUser, turnID: ack.TurnID, body: msg.cmd.Text}
			if ack.Status == conversation.AckQueued {
				item.tag = fmt.Sprintf("queued #%d", ack.Position)
				m.queued++
			} else if m.phase == phaseIdle {
				m.phase = phaseRunning
				m.activeTurn = ack.TurnID
			}
			m.transcript.add(item)
			// Sending a message returns the view to the newest content.
			m.transcript.vp.GotoBottom()
		default:
			m.notice = "not sent: " + ack.Reason
		}
	case conversation.CommandCancelTurn:
		if ack.Status == conversation.AckAccepted {
			m.phase = phaseCancelling
			m.notice = "cancelling — Ctrl+C again to quit"
		} else {
			m.notice = ack.Reason
		}
	case conversation.CommandWrapUp:
		if ack.Status == conversation.AckAccepted {
			m.notice = "wrapping up — the coordinator will summarize and finish"
		} else {
			m.notice = ack.Reason
		}
	default:
		if ack.Status != conversation.AckAccepted {
			m.notice = ack.Reason
		}
	}
	return m
}

func (m ChatModel) handleEvent(ev conversation.Event) ChatModel {
	switch ev := ev.(type) {
	case conversation.TurnStarted:
		m.phase = phaseRunning
		m.activeTurn = ev.TurnID
		m.turnStartedAt, m.now = ev.StartedAt, ev.StartedAt
		m.dash.statusText = "" // activity from an earlier turn is stale
		if i := m.transcript.findTurnItem(itemUser, ev.TurnID); i >= 0 && m.transcript.items[i].tag != "" {
			m.transcript.items[i].tag = ""
			m.transcript.items[i].version++
			m.queued = max(m.queued-1, 0)
		}
		m.turnCount++
		card := newProgressCard(ev.TurnID, m.turnCount, m.dash.tasks)
		card.refresh(m.dash.tasks, m.dash.logs)
		m.transcript.add(chatItem{kind: itemProgress, turnID: ev.TurnID, card: card})
	case conversation.TurnSettled:
		m = m.settleTurn(ev.Result)
	case conversation.QueueDropped:
		for _, turnID := range ev.TurnIDs {
			if i := m.transcript.findTurnItem(itemUser, turnID); i >= 0 {
				m.transcript.items[i].tag = "dropped: session closed"
				m.transcript.items[i].version++
			}
		}
		m.queued = 0
	case conversation.ControlDone:
		m = m.handleControlDone(ev)
	case conversation.Closed:
		m.phase = phaseClosing
	}
	return m
}

func (m ChatModel) settleTurn(result conversation.TurnResult) ChatModel {
	label := turnOutcomeLabel(result)
	if i := m.transcript.findTurnItem(itemProgress, result.TurnID); i >= 0 {
		card := m.transcript.items[i].card
		card.refresh(m.dash.tasks, m.dash.logs)
		card.freeze(label)
		m.transcript.items[i].version++
	}
	title := "Coordinator · " + label
	if result.Elapsed > 0 {
		title += " · " + formatElapsed(result.Elapsed)
	}
	item := chatItem{kind: itemAssistant, turnID: result.TurnID, title: title, body: result.Answer}
	if result.Result != nil {
		item.subtitle = team.FormatCanonicalStatus(result.Result)
	}
	if result.Answer != "" || result.Result != nil {
		m.transcript.add(item)
	}
	switch {
	case result.Cancelled:
		m.transcript.add(chatItem{kind: itemNotice, turnID: result.TurnID, body: "Turn cancelled."})
	case result.Err != nil:
		m.transcript.add(chatItem{kind: itemError, turnID: result.TurnID, body: "Error: " + result.Err.Error()})
	}
	m.lastResult = result.Result
	m.lastTurnID = result.TurnID
	m.lastRunIDs = append([]string(nil), result.RunIDs...)
	if m.activeTurn == result.TurnID {
		m.activeTurn = ""
		m.phase = phaseIdle
	}
	if m.notice == "cancelling — Ctrl+C again to quit" || strings.HasPrefix(m.notice, "wrapping up") {
		m.notice = ""
	}
	return m
}

// turnOutcomeLabel names a settled turn using only runtime-provided facts.
func turnOutcomeLabel(result conversation.TurnResult) string {
	switch {
	case result.Cancelled:
		return "cancelled"
	case result.Result != nil:
		return string(result.Result.Outcome)
	case result.Err != nil:
		return "error"
	default:
		return "no result"
	}
}

func (m *ChatModel) refreshLiveCard() {
	if m.activeTurn == "" {
		return
	}
	if i := m.transcript.findTurnItem(itemProgress, m.activeTurn); i >= 0 {
		if m.transcript.items[i].card.refresh(m.dash.tasks, m.dash.logs) {
			m.transcript.items[i].version++
		}
	}
}

func (m *ChatModel) openTextOverlay(title, body string) {
	m.overlay = overlayText
	m.overlayTitle = title
	m.overlayBody = body
	m.overlayLines = 0
}

func (m ChatModel) focusComposer() (ChatModel, tea.Cmd) {
	m.focus = focusComposer
	cmd := m.composer.Focus()
	if m.dash.displayPreset == DisplayEpaper {
		cmd = nil
	}
	return m, cmd
}

func (m ChatModel) cycleTheme() (ChatModel, tea.Cmd) {
	var theme string
	m.dash, theme = m.dash.cycleTheme()
	m.dash.themeGeneration++
	m.notice = "Theme: " + theme
	return m.afterThemeChange()
}

func (m ChatModel) afterThemeChange() (ChatModel, tea.Cmd) {
	applyComposerStyles(&m.composer, m.dash.styles)
	m.rerender()
	if m.dash.themeMode == ThemeAuto {
		return m, tea.Batch(tea.ClearScreen, m.dash.watchThemeCmd())
	}
	return m, tea.ClearScreen
}

// requestExit asks for confirmation when work would be cancelled or
// dropped, and closes immediately otherwise.
func (m ChatModel) requestExit() (ChatModel, tea.Cmd) {
	if m.phase == phaseIdle && m.queued == 0 {
		return m.startClose()
	}
	m.overlay = overlayConfirmExit
	return m, nil
}

func (m ChatModel) startClose() (ChatModel, tea.Cmd) {
	if m.phase == phaseClosing {
		return m, nil
	}
	m.phase = phaseClosing
	m.overlay = overlayNone
	m.notice = "closing session…"
	closeFn := m.actions.Close
	if closeFn == nil {
		return m, tea.Quit
	}
	return m, func() tea.Msg { return chatClosedMsg{report: closeFn()} }
}
