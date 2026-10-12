package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/team"
)

func TestChatHistoryConvertsSessionEntries(t *testing.T) {
	if got := chatHistory(nil); got != nil {
		t.Fatalf("chatHistory(nil) = %v, want nil", got)
	}
	got := chatHistory(&team.SessionData{Entries: []team.SessionEntry{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}})
	if len(got) != 2 || got[0].Role != "user" || got[1].Content != "a" {
		t.Fatalf("chatHistory = %+v", got)
	}
}

func TestChatTeamHooks(t *testing.T) {
	session := &chatSession{rootCtx: context.Background()} // nil registry: --default
	alpha := &teamContext{teamName: "alpha", coordinator: &team.Coordinator{}}
	adapter := newChatRuntimeAdapter(alpha, "alpha")
	var sent []tea.Msg
	session.installChatTeamHooks(adapter, &chatDisplay{}, func(msg tea.Msg) { sent = append(sent, msg) })

	if err := adapter.SwitchTeam(context.Background(), "beta"); err == nil || !strings.Contains(err.Error(), "default team") {
		t.Fatalf("switch without a registry = %v, want the default-team error", err)
	}
	if current, name := adapter.Current(); current != alpha || name != "alpha" {
		t.Fatalf("a failed switch replaced the team: %q", name)
	}
	if len(sent) != 0 {
		t.Fatalf("a failed switch sent UI messages: %v", sent)
	}
	if err := adapter.ResetContext(context.Background()); err != nil {
		t.Fatalf("reset = %v", err)
	}
}

func TestChatCostMatchesInspectCost(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	tests := []struct {
		name    string
		seedRun bool
		wantErr bool
	}{
		{name: "recorded run", seedRun: true},
		{name: "unknown run", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tt.seedRun {
				store, err := team.NewEventStore(workspace, "run-123", "session-1")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.AppendPersisted(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"cost"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			chatOutput, chatErr := renderChatCost(workspace, "run-123")

			root := newRootCommand()
			var inspectOutput bytes.Buffer
			root.SetOut(&inspectOutput)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"inspect", "cost", "run-123", "--workspace", workspace, "--format", "text"})
			inspectErr := root.Execute()

			if (chatErr != nil) != tt.wantErr || (inspectErr != nil) != tt.wantErr {
				t.Fatalf("chat err = %v, inspect err = %v, want error %v", chatErr, inspectErr, tt.wantErr)
			}
			if !tt.wantErr && (chatOutput == "" || chatOutput != inspectOutput.String()) {
				t.Fatalf("/cost differs from hufu inspect cost:\nchat:\n%s\ninspect:\n%s", chatOutput, inspectOutput.String())
			}
		})
	}
}
