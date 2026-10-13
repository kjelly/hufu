package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestSelectChatUIMode(t *testing.T) {
	tests := []struct {
		name      string
		ui        string
		uiChanged bool
		tui       bool
		terminal  bool
		want      chatUIMode
		wantErr   string
	}{
		{name: "default plain", ui: "plain", terminal: true, want: chatUIPlain},
		{name: "dashboard", ui: "dashboard", uiChanged: true, terminal: true, want: chatUIDashboard},
		{name: "chat", ui: "chat", uiChanged: true, terminal: true, want: chatUIChat},
		{name: "tui alias", ui: "plain", tui: true, terminal: true, want: chatUIDashboard},
		{name: "tui with explicit dashboard", ui: "dashboard", uiChanged: true, tui: true, terminal: true, want: chatUIDashboard},
		{name: "tui conflicts with chat", ui: "chat", uiChanged: true, tui: true, terminal: true, wantErr: "--tui conflicts with --ui chat"},
		{name: "tui conflicts with plain", ui: "plain", uiChanged: true, tui: true, terminal: true, wantErr: "--tui conflicts with --ui plain"},
		{name: "invalid value", ui: "web", uiChanged: true, terminal: true, wantErr: `invalid --ui "web"`},
		{name: "chat needs a terminal", ui: "chat", uiChanged: true, terminal: false, wantErr: "requires an interactive terminal"},
		{name: "plain works without a terminal", ui: "plain", terminal: false, want: chatUIPlain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousUI, previousTUI, previousTerminal := opts.chatUI, opts.tuiMode, chatUITerminal
			t.Cleanup(func() { opts.chatUI, opts.tuiMode, chatUITerminal = previousUI, previousTUI, previousTerminal })
			opts.chatUI, opts.tuiMode = tt.ui, tt.tui
			chatUITerminal = func() bool { return tt.terminal }
			got, err := selectChatUIMode(tt.uiChanged)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("mode = %q, err = %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestChatCommandRegistersDisplayFlags(t *testing.T) {
	for _, name := range []string{"ui", "tui", "theme", "display-preset", "no-spinner"} {
		if replCmd.Flags().Lookup(name) == nil {
			t.Fatalf("chat command does not register --%s", name)
		}
	}
	if got := replCmd.Flags().Lookup("ui").DefValue; got != "plain" {
		t.Fatalf("--ui default = %q, want plain", got)
	}
}

func TestChatAgentEntriesUseResolvedModels(t *testing.T) {
	tests := []struct {
		name        string
		config      agent.TeamConfig
		roleSources []roleModelSource
		agents      []*agent.AgentDef
		want        map[string]string
	}{
		{
			name:   "resolved coordinator and worker defaults",
			config: agent.TeamConfig{CoordinatorModel: "ollama/coord", WorkerModel: "ollama/worker"},
			agents: []*agent.AgentDef{
				{Name: "coordinator", Role: "coordinator"},
				{Name: "reviewer", Role: "worker"},
				{Name: "critic", Role: "worker", Generation: agent.GenerationParams{Model: "codex/gpt"}},
			},
			want: map[string]string{"coordinator": "ollama/coord", "reviewer": "ollama/worker", "critic": "codex/gpt"},
		},
		{
			name:   "coordinator target wins over the agent file, as at runtime",
			config: agent.TeamConfig{CoordinatorModel: "ollama/coord", WorkerModel: "ollama/worker"},
			agents: []*agent.AgentDef{{Name: "lead", Role: "coordinator", Generation: agent.GenerationParams{Model: "ollama/other"}}},
			want:   map[string]string{"lead": "ollama/coord"},
		},
		{
			name:        "team route is the worker default",
			config:      agent.TeamConfig{CoordinatorModel: "ollama/coord", ExecutionRoute: "fast"},
			roleSources: []roleModelSource{{Role: "Worker", Route: "fast", Candidates: []string{"ollama/a", "ollama/b"}}},
			agents:      []*agent.AgentDef{{Name: "helper", Role: "worker"}},
			want:        map[string]string{"helper": "route fast → ollama/a, ollama/b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agents := map[string]*agent.AgentDef{}
			for _, def := range tt.agents {
				agents[def.Name] = def
			}
			tc := &teamContext{session: &team.TeamSession{Config: tt.config, Agents: agents}, roleSources: tt.roleSources}
			got := map[string]string{}
			for _, entry := range chatAgentEntries(tc) {
				got[entry.Name] = entry.Model
			}
			for name, model := range tt.want {
				if got[name] != model {
					t.Fatalf("%s model = %q, want %q (all: %v)", name, got[name], model, got)
				}
			}
		})
	}
}
