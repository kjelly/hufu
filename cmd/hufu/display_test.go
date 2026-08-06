package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

func TestJSONStatusEventMarshals(t *testing.T) {
	data, err := json.Marshal(jsonStatusEvent{Type: "start", Agent: "worker", Time: "2026-01-01T00:00:00Z"})
	if err != nil || !strings.Contains(string(data), `"type":"start"`) {
		t.Fatalf("json = %q, err = %v", data, err)
	}
}

type testStatusWriter struct {
	b strings.Builder
}

func (w *testStatusWriter) write(s string) {
	w.b.WriteString(s)
}

func TestDispatchStatusEventBuffersReasoningAfterMainOutput(t *testing.T) {
	w := &testStatusWriter{}
	st := &reporterState{}

	dispatchStatusEvent(w, st, team.StatusEvent{Type: "start", Agent: "helper", Message: "task"})
	dispatchStatusEvent(w, st, team.StatusEvent{Type: "reasoning", Agent: "helper", Message: "thinking first"})
	dispatchStatusEvent(w, st, team.StatusEvent{Type: "text", Agent: "helper", Message: "final answer"})
	dispatchStatusEvent(w, st, team.StatusEvent{Type: "done", Agent: "helper"})

	out := w.b.String()
	textIdx := strings.Index(out, "final answer")
	thinkIdx := strings.Index(out, "thinking first")
	if textIdx < 0 {
		t.Fatalf("expected final answer in output, got: %q", out)
	}
	if thinkIdx < 0 {
		t.Fatalf("expected reasoning in output, got: %q", out)
	}
	if thinkIdx < textIdx {
		t.Fatalf("expected reasoning after main output, got: %q", out)
	}
}

func TestDispatchStatusEventShowsBudgetExceeded(t *testing.T) {
	w := &testStatusWriter{}
	st := &reporterState{}

	dispatchStatusEvent(w, st, team.StatusEvent{Type: "budget_exceeded", Message: "wall-clock budget exceeded (11m > 10m)"})

	out := w.b.String()
	if !strings.Contains(out, "budget exceeded") {
		t.Fatalf("expected budget exceeded marker, got: %q", out)
	}
	if !strings.Contains(out, "wall-clock budget exceeded") {
		t.Fatalf("expected budget reason, got: %q", out)
	}
}

func TestDispatchStatusEventShowsTaskTimeout(t *testing.T) {
	w := &testStatusWriter{}
	st := &reporterState{}

	dispatchStatusEvent(w, st, team.StatusEvent{
		Type:      "task_timeout",
		Agent:     "helper",
		Message:   "attempt 1 timed out after 2m",
		Duration:  2 * time.Minute,
		ModelTime: 90 * time.Second,
		ToolTime:  30 * time.Second,
	})

	out := w.b.String()
	if !strings.Contains(out, "timed out after 2m") {
		t.Fatalf("expected timeout message, got: %q", out)
	}
	if !strings.Contains(out, "helper") {
		t.Fatalf("expected agent label, got: %q", out)
	}
}

func TestWrapPreviewLinesWideEnough(t *testing.T) {
	got := wrapPreviewLines("go run ./cmd/tool inspect --name host-a -- cat /etc/ssh/sshd_config", 120, 4)
	if len(got) != 1 {
		t.Fatalf("expected one line, got %#v", got)
	}
	if strings.Contains(got[0], "...") {
		t.Fatalf("did not expect ellipsis in %q", got[0])
	}
}

func TestWrapPreviewLinesWrapsWithoutPrematureEllipsis(t *testing.T) {
	got := wrapPreviewLines("go run ./cmd/tool inspect --name host-a -- cat /etc/ssh/sshd_config", 36, 4)
	if len(got) < 2 {
		t.Fatalf("expected wrapped output, got %#v", got)
	}
	if strings.Contains(got[0], "...") {
		t.Fatalf("did not expect ellipsis on first line: %#v", got)
	}
	if strings.Contains(strings.Join(got, "\n"), "...") {
		t.Fatalf("did not expect ellipsis while within max lines: %#v", got)
	}
}

func TestShouldRedrawTaskDisplay(t *testing.T) {
	tests := []struct {
		mode       string
		isTerminal bool
		want       bool
	}{
		{mode: "auto", isTerminal: true, want: true},
		{mode: "auto", isTerminal: false, want: false},
		{mode: "terminal", isTerminal: false, want: true},
		{mode: "plain", isTerminal: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if got := shouldRedrawTaskDisplay(tt.mode, tt.isTerminal); got != tt.want {
				t.Errorf("shouldRedrawTaskDisplay(%q, %t) = %t, want %t", tt.mode, tt.isTerminal, got, tt.want)
			}
		})
	}
}

func TestShouldDisableColor(t *testing.T) {
	tests := []struct {
		name       string
		noColor    bool
		format     string
		noColorEnv string
		want       bool
	}{
		{name: "default", want: false},
		{name: "flag", noColor: true, want: true},
		{name: "environment", noColorEnv: "1", want: true},
		{name: "json", format: "json", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDisableColor(tt.noColor, tt.format, tt.noColorEnv); got != tt.want {
				t.Errorf("shouldDisableColor(%t, %q, %q) = %t, want %t", tt.noColor, tt.format, tt.noColorEnv, got, tt.want)
			}
		})
	}
}
