package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
		{name: "queue full", submit: func(string) bool { return false }, wantCmd: true, wantOpen: true, wantDraft: "fix it", wantNotice: true, wantCallCount: 1},
		{name: "injection unavailable", submit: nil, wantCmd: false, wantOpen: true, wantDraft: "fix it", wantNotice: true, wantCallCount: 0},
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
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
			m = next.(Model)
			m.promptInput.SetValue("fix it")

			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(Model)
			if calls != 0 {
				t.Fatalf("Update called SubmitPrompt directly (%d calls); it must run in a command", calls)
			}
			if m.promptInput.Value() != "fix it" {
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
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected submit command")
	}
	m = next.(Model)
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if second != nil {
		t.Error("second enter while submitting must not submit again")
	}
}
