package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestPromptInputSubmitKeepsDraftUntilAccepted(t *testing.T) {
	tests := []struct {
		name          string
		submit        func(string) bool
		wantCmd       bool
		wantOpen      bool
		wantDraft     string
		wantNotice    bool
		wantCallCount int
	}{
		{name: "accepted", submit: func(string) bool { return true }, wantCmd: true, wantOpen: false, wantDraft: "", wantCallCount: 1},
		{name: "queue full", submit: func(string) bool { return false }, wantCmd: true, wantOpen: true, wantDraft: "fix it\nwith tests", wantNotice: true, wantCallCount: 1},
		{name: "injection unavailable", submit: nil, wantCmd: false, wantOpen: true, wantDraft: "fix it\nwith tests", wantNotice: true, wantCallCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("test", TeamInfo{TeamName: "t"})
			m.width, m.height = 100, 30
			calls := 0
			if tt.submit != nil {
				m.SubmitPrompt = func(prompt string) bool {
					calls++
					return tt.submit(prompt)
				}
			}
			next, _ := m.Update(keyPress("c"))
			m = next.(Model)
			m.promptInput.SetValue("fix it\nwith tests")

			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(Model)
			if calls != 0 {
				t.Fatalf("Update called SubmitPrompt directly (%d calls); it must run in a command", calls)
			}
			if m.promptInput.Value() != "fix it\nwith tests" {
				t.Fatalf("draft cleared before acceptance: %q", m.promptInput.Value())
			}
			if (cmd != nil) != tt.wantCmd {
				t.Fatalf("command returned = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if cmd != nil {
				next, _ = m.Update(cmd())
				m = next.(Model)
			}
			if calls != tt.wantCallCount {
				t.Errorf("SubmitPrompt calls = %d, want %d", calls, tt.wantCallCount)
			}
			if m.inPromptInput != tt.wantOpen {
				t.Errorf("inPromptInput = %v, want %v", m.inPromptInput, tt.wantOpen)
			}
			if got := m.promptInput.Value(); got != tt.wantDraft {
				t.Errorf("draft = %q, want %q", got, tt.wantDraft)
			}
			if (m.promptNotice != "") != tt.wantNotice {
				t.Errorf("promptNotice = %q, want notice %v", m.promptNotice, tt.wantNotice)
			}
			if m.promptSubmitting {
				t.Error("promptSubmitting still set after the submit result")
			}
		})
	}
}

func TestPromptInputIgnoresEnterWhileSubmitting(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.SubmitPrompt = func(string) bool { return true }
	m.inPromptInput = true
	m.promptInput.SetValue("first")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected submit command")
	}
	m = next.(Model)
	_, second := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if second != nil {
		t.Error("second enter while submitting must not submit again")
	}
}

func TestPromptInputMultiline(t *testing.T) {
	for _, newline := range []tea.KeyPressMsg{
		{Code: 'j', Mod: tea.ModCtrl},
		{Code: tea.KeyEnter, Mod: tea.ModAlt},
		{Code: tea.KeyEnter, Mod: tea.ModShift},
	} {
		t.Run(newline.String(), func(t *testing.T) {
			m := New("test", TeamInfo{})
			m.width, m.height = 100, 30
			m.SubmitPrompt = func(prompt string) bool {
				if prompt != "first\n第二行\n\n  last" {
					t.Errorf("submitted task = %q", prompt)
				}
				return true
			}
			next, _ := m.Update(keyPress("c"))
			m = next.(Model)
			next, _ = m.Update(tea.PasteMsg{Content: "first"})
			m = next.(Model)
			next, _ = m.Update(newline)
			m = next.(Model)
			next, _ = m.Update(tea.PasteMsg{Content: "第二行\n\n  last"})
			m = next.(Model)
			if !m.inPromptInput || m.promptSubmitting || m.promptInput.Value() != "first\n第二行\n\n  last" {
				t.Fatalf("newline or paste submitted or altered draft: %q", m.promptInput.Value())
			}
			next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = next.(Model)
			if cmd == nil {
				t.Fatal("expected submission")
			}
			next, _ = m.Update(cmd())
			if next.(Model).inPromptInput {
				t.Fatal("accepted multiline task did not close input")
			}
		})
	}
}

func TestPromptInputLongPasteAndResize(t *testing.T) {
	m := New("test", TeamInfo{})
	m.width, m.height = 100, 30
	next, _ := m.Update(keyPress("c"))
	m = next.(Model)
	draft := strings.Repeat("a long task line\n", 30) + "last"
	next, _ = m.Update(tea.PasteMsg{Content: draft})
	m = next.(Model)
	if m.promptInput.Value() != draft {
		t.Fatal("long paste was truncated")
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 65, Height: 18})
	m = next.(Model)
	if m.promptInput.Height() > 8 || m.promptInput.Value() != draft {
		t.Fatal("resize lost draft or exceeded available input height")
	}
	view := m.promptInputView()
	if !strings.Contains(view, "last") || !strings.Contains(view, "newline") {
		t.Fatal("scrolled input must show cursor line and newline help")
	}
	next, _ = m.Update(tea.PasteMsg{Content: strings.Repeat("\n", maxComposerLines)})
	m = next.(Model)
	if m.promptInput.Value() != draft || m.promptNotice == "" {
		t.Fatal("over-limit paste must be refused without changing draft")
	}
}
