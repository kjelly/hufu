package notify

import (
	"bytes"
	"strings"
	"testing"
)

func TestNotifyConfigShouldNotify(t *testing.T) {
	tests := []struct {
		name      string
		events    []string
		eventType string
		want      bool
	}{
		{"matching event", []string{"done", "error"}, "done", true},
		{"non-matching event", []string{"done", "error"}, "start", false},
		{"empty events means all disabled", []string{}, "done", false},
		{"single event match", []string{"wrap_up"}, "wrap_up", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NotifyConfig{Events: tt.events}
			if got := cfg.ShouldNotify(tt.eventType); got != tt.want {
				t.Errorf("ShouldNotify(%q) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestNotifyConfigEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  NotifyConfig
		want bool
	}{
		{"osc enabled", NotifyConfig{OSC: true}, true},
		{"command enabled", NotifyConfig{Command: "notify-send"}, true},
		{"both enabled", NotifyConfig{OSC: true, Command: "notify-send"}, true},
		{"neither enabled", NotifyConfig{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNotifySkipsFilteredEvents(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifier(NotifyConfig{
		OSC:    true,
		Events: []string{"done"},
	}, &buf)

	n.Notify("start", "dev", "hello", "")
	if buf.Len() > 0 {
		t.Errorf("expected no output for filtered event, got %q", buf.String())
	}
}

func TestNotifySendsOSC(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifier(NotifyConfig{
		OSC:    true,
		Events: []string{"done"},
	}, &buf)

	n.Notify("done", "dev", "task complete", "")
	output := buf.String()

	if !strings.Contains(output, "\x07") {
		t.Error("expected BEL character in output")
	}
	if !strings.Contains(output, "\x1b]777;notify;") {
		t.Error("expected OSC 777 sequence in output")
	}
	if !strings.Contains(output, "\x1b]9;") {
		t.Error("expected OSC 9 sequence in output")
	}
}

func TestNotifyDisabled(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifier(NotifyConfig{
		OSC:    false,
		Events: []string{"done"},
	}, &buf)

	n.Notify("done", "dev", "task complete", "")
	if buf.Len() > 0 {
		t.Errorf("expected no output when disabled, got %q", buf.String())
	}
}

func TestFormatEvent(t *testing.T) {
	tests := []struct {
		name       string
		eventType  string
		agent      string
		message    string
		output     string
		wantTitle  string
		wantMsgHas string
	}{
		{
			name: "done with output", eventType: "done", agent: "dev", message: "",
			output: "result text", wantTitle: "hufu - done", wantMsgHas: "dev finished: result text",
		},
		{
			name: "done without output", eventType: "done", agent: "dev", message: "",
			output: "", wantTitle: "hufu - done", wantMsgHas: "dev done",
		},
		{
			name: "error", eventType: "error", agent: "dev", message: "failed",
			output: "", wantTitle: "hufu - error", wantMsgHas: "dev: failed",
		},
		{
			name: "wrap_up", eventType: "wrap_up", agent: "", message: "",
			output: "", wantTitle: "hufu", wantMsgHas: "Wrap up requested",
		},
		{
			name: "start", eventType: "start", agent: "dev", message: "research",
			output: "", wantTitle: "hufu - start", wantMsgHas: "dev starting: research",
		},
		{
			name: "unknown event type", eventType: "custom", agent: "", message: "hello",
			output: "", wantTitle: "hufu", wantMsgHas: "custom: hello",
		},
		{
			name: "no agent uses coordinator", eventType: "done", agent: "", message: "",
			output: "done", wantTitle: "hufu - done", wantMsgHas: "coordinator finished: done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, message := formatEvent(tt.eventType, tt.agent, tt.message, tt.output)
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if !strings.Contains(message, tt.wantMsgHas) {
				t.Errorf("message = %q, want it to contain %q", message, tt.wantMsgHas)
			}
		})
	}
}

func TestEscapeOSC(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello", "hello"},
		{"semicolon", "a;b", "a\\;b"},
		{"backslash", `a\b`, `a\\b`},
		{"bell char", "a\x07b", "a\\ab"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeOSC(tt.input); got != tt.want {
				t.Errorf("escapeOSC(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.OSC {
		t.Error("expected OSC to be true in default config")
	}
	if len(cfg.Events) != len(defaultEvents) {
		t.Errorf("expected %d default events, got %d", len(defaultEvents), len(cfg.Events))
	}
	for i, e := range defaultEvents {
		if cfg.Events[i] != e {
			t.Errorf("Events[%d] = %q, want %q", i, cfg.Events[i], e)
		}
	}
}

func TestNewNotifierFillsDefaultEvents(t *testing.T) {
	var buf bytes.Buffer
	n := NewNotifier(NotifyConfig{OSC: true, Events: nil}, &buf)
	if len(n.cfg.Events) != len(defaultEvents) {
		t.Errorf("expected %d default events, got %d", len(defaultEvents), len(n.cfg.Events))
	}
	for i, e := range defaultEvents {
		if n.cfg.Events[i] != e {
			t.Errorf("Events[%d] = %q, want %q", i, n.cfg.Events[i], e)
		}
	}
}
