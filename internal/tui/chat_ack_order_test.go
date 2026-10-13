package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

// pendingSubmit types text and presses Enter but returns the Ack instead of
// delivering it, so a test can deliver controller events first. Acks and
// events reach the program on different goroutines in either order.
func pendingSubmit(t *testing.T, m ChatModel, text string) (ChatModel, tea.Msg) {
	t.Helper()
	m.composer.SetValue(text)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	return next.(ChatModel), cmd()
}

func TestChatLatePromptAckNeverOverridesEvents(t *testing.T) {
	started := conversation.TurnStarted{TurnID: "turn-1", Prompt: "fix the bug"}
	settled := conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "turn-1", Answer: "done", Result: &team.RunResult{Outcome: team.RunOutcomeCompleted}}}
	tests := []struct {
		name       string
		status     conversation.AckStatus
		before     []conversation.Event // delivered before the Ack
		wantPhase  chatPhase
		wantActive string
		wantQueued int
		wantTag    string
	}{
		{name: "accepted after started", status: conversation.AckAccepted, before: []conversation.Event{started}, wantPhase: phaseRunning, wantActive: "turn-1"},
		{name: "accepted after settled", status: conversation.AckAccepted, before: []conversation.Event{started, settled}, wantPhase: phaseIdle},
		{name: "queued after its turn started", status: conversation.AckQueued, before: []conversation.Event{started}, wantPhase: phaseRunning, wantActive: "turn-1"},
		{name: "queued after its turn settled", status: conversation.AckQueued, before: []conversation.Event{started, settled}, wantPhase: phaseIdle},
		{name: "queued after the queue was dropped", status: conversation.AckQueued, before: []conversation.Event{conversation.QueueDropped{TurnIDs: []string{"turn-1"}}}, wantPhase: phaseIdle, wantTag: droppedTag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, runtime := newTestChat(t, 100, 30)
			runtime.next = tt.status
			m, ack := pendingSubmit(t, m, "fix the bug")
			for _, ev := range tt.before {
				m = chatEvent(t, m, ev)
			}
			m = chatStep(t, m, ack)

			if m.phase != tt.wantPhase || m.activeTurn != tt.wantActive {
				t.Fatalf("phase=%v active=%q, want phase=%v active=%q", m.phase, m.activeTurn, tt.wantPhase, tt.wantActive)
			}
			if m.queued != tt.wantQueued {
				t.Fatalf("queued = %d, want %d", m.queued, tt.wantQueued)
			}
			if m.submitting != "" || m.composer.Value() != "" {
				t.Fatalf("admitted draft not released: submitting=%q composer=%q", m.submitting, m.composer.Value())
			}
			users, user, progress := 0, -1, m.transcript.findTurnItem(itemProgress, "turn-1")
			for i, item := range m.transcript.items {
				if item.kind == itemUser && item.turnID == "turn-1" {
					users, user = users+1, i
				}
			}
			if users != 1 {
				t.Fatalf("prompt shown %d times, want once", users)
			}
			if progress >= 0 && user > progress {
				t.Fatalf("prompt at %d follows its progress card at %d", user, progress)
			}
			if tag := m.transcript.items[user].tag; tag != tt.wantTag {
				t.Fatalf("prompt tag = %q, want %q", tag, tt.wantTag)
			}
		})
	}
}

func TestChatLateControlAcksKeepSettledState(t *testing.T) {
	controlAck := func(kind conversation.CommandKind, turnID string) chatAckMsg {
		return chatAckMsg{
			cmd: conversation.Command{ID: "control", Kind: kind},
			ack: conversation.Ack{CommandID: "control", TurnID: turnID, Status: conversation.AckAccepted},
		}
	}
	m, _ := newTestChat(t, 100, 30)
	m = chatSubmit(t, m, "first")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1", Prompt: "first"})
	m = settle(t, m, "turn-1", "", nil, nil, true)

	for _, kind := range []conversation.CommandKind{conversation.CommandCancelTurn, conversation.CommandWrapUp} {
		m = chatStep(t, m, controlAck(kind, "turn-1"))
		if m.phase != phaseIdle || m.notice != "" {
			t.Fatalf("late %s ack after settlement: phase=%v notice=%q", kind, m.phase, m.notice)
		}
	}

	// A late Ack for turn-1 leaves the newer running turn alone.
	m = chatSubmit(t, m, "second")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-2", Prompt: "second"})
	m = chatStep(t, m, controlAck(conversation.CommandCancelTurn, "turn-1"))
	if m.phase != phaseRunning || m.activeTurn != "turn-2" {
		t.Fatalf("late cancel for turn-1 changed turn-2: phase=%v active=%q", m.phase, m.activeTurn)
	}
	m = chatStep(t, m, controlAck(conversation.CommandCancelTurn, "turn-2"))
	if m.phase != phaseCancelling || m.notice != cancellingNotice {
		t.Fatalf("cancel for the running turn: phase=%v notice=%q", m.phase, m.notice)
	}
}
