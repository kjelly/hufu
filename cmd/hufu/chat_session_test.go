package main

import (
	"strings"
	"testing"
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
