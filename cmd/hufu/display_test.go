package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/team"
)

func TestJSONStatusEventMarshals(t *testing.T) {
	data, err := json.Marshal(jsonStatusEvent{Type: "start", Agent: "worker", Time: "2026-01-01T00:00:00Z", ExecutionTarget: "codex/gpt-5.6-luna", Backend: "codex", BackendKind: "agent"})
	if err != nil || !strings.Contains(string(data), `"type":"start"`) || !strings.Contains(string(data), `"execution_target":"codex/gpt-5.6-luna"`) || !strings.Contains(string(data), `"backend_kind":"agent"`) {
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

func TestDispatchStatusEventRendersSecretFreeModelProfile(t *testing.T) {
	w := &testStatusWriter{}
	dispatchStatusEvent(w, &reporterState{}, team.StatusEvent{Type: "model_profile_resolved", ModelProfile: &modelprofile.TelemetryProjection{
		Provider: "ollama", ModelID: "qwen3:8b", Effective: modelprofile.TelemetryValue[int]{Value: 32_768, Source: modelprofile.SourceProviderRuntime},
	}})
	if got := w.b.String(); !strings.Contains(got, "model profile") || !strings.Contains(got, "32768") || strings.Contains(got, "http") {
		t.Fatalf("profile status = %q", got)
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

func TestDispatchStatusEventShowsCodexActivity(t *testing.T) {
	w := &testStatusWriter{}
	dispatchStatusEvent(w, &reporterState{}, team.StatusEvent{
		Type: "codex_activity", Agent: "coder", Model: "codex/gpt-5.6-luna", Message: "Codex is still working (10s elapsed)",
	})

	out := w.b.String()
	if !strings.Contains(out, "coder") || !strings.Contains(out, "Codex is still working") {
		t.Fatalf("expected Codex activity in output, got: %q", out)
	}
}

func TestDispatchStatusEventWrapsCodexActivityDetails(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	w := &testStatusWriter{}
	dispatchStatusEvent(w, &reporterState{}, team.StatusEvent{
		Type:    "codex_activity",
		Agent:   "reviewer",
		Model:   "codex/gpt-5.6-luna",
		Message: `Codex item completed (commandExecution): command="go test ./..." output="all tests passed"`,
	})

	out := w.b.String()
	if !strings.Contains(out, "Codex") || !strings.Contains(out, "command=") || !strings.Contains(out, "passed") {
		t.Fatalf("expected wrapped Codex detail in output, got: %q", out)
	}
	assertPhysicalLineWidths(t, out, 40)
}

func TestDispatchStatusEventCodexActivityFitsTerminalWidth(t *testing.T) {
	const width = 40
	t.Setenv("COLUMNS", "40")

	tests := []struct {
		name      string
		event     team.StatusEvent
		wantInOut string
	}{
		{
			name:      "dynamic agent and model label",
			wantInOut: "Codex",
			event: team.StatusEvent{
				Type: "codex_activity", Agent: "reviewer", Model: "codex/gpt-5.6-luna",
				Message: `Codex item completed (commandExecution): command="go test ./..." output="all tests passed"`,
			},
		},
		{
			name:      "overlong label",
			wantInOut: "…",
			event: team.StatusEvent{
				Type: "codex_activity", TeamName: "very-long-team-name", Agent: "very-long-agent-name-that-needs-truncation", Model: "codex/gpt-5.6-luna",
				Message: "activity",
			},
		},
		{
			name:      "wide unicode payload",
			wantInOut: "界",
			event: team.StatusEvent{
				Type: "codex_activity", Agent: "reviewer", Model: "codex/gpt-5.6-luna",
				Message: strings.Repeat("界", 40),
			},
		},
		{
			name:      "unbroken payload",
			wantInOut: "unbroken",
			event: team.StatusEvent{
				Type: "codex_activity", Agent: "reviewer", Model: "codex/gpt-5.6-luna",
				Message: strings.Repeat("unbroken-token-", 20),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &testStatusWriter{}
			dispatchStatusEvent(w, &reporterState{}, tt.event)
			if out := w.b.String(); out == "" {
				t.Fatal("expected Codex activity output")
			} else {
				if !strings.Contains(out, tt.wantInOut) {
					t.Fatalf("expected output to contain %q, got: %q", tt.wantInOut, out)
				}
				assertPhysicalLineWidths(t, out, width)
			}
		})
	}
}

func TestDispatchStatusEventAdvancesANSIStyledCodexActivity(t *testing.T) {
	const width = 40
	t.Setenv("COLUMNS", "40")

	message := "\x1b[31m" + strings.Repeat("x", 90) + "FINISHED" + "\x1b[0m"
	w := &testStatusWriter{}
	dispatchStatusEvent(w, &reporterState{}, team.StatusEvent{
		Type: "codex_activity", Agent: "reviewer", Model: "codex/gpt-5.6-luna", Message: message,
	})

	out := w.b.String()
	if !strings.Contains(ansi.Strip(out), "FINISHED") {
		t.Fatalf("expected ANSI-styled activity to advance through its final content, got: %q", out)
	}
	assertPhysicalLineWidths(t, out, width)
}

func assertPhysicalLineWidths(t *testing.T, output string, width int) {
	t.Helper()
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d: %q", lineNumber+1, got, width, line)
		}
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

func TestWrapPreviewLinesTruncatesWithinDisplayWidth(t *testing.T) {
	got := wrapPreviewLines(strings.Repeat("overflow ", 10), 5, 2)
	if len(got) != 2 {
		t.Fatalf("expected two truncated lines, got %#v", got)
	}
	if !strings.HasSuffix(got[len(got)-1], "...") {
		t.Fatalf("expected truncation marker, got %#v", got)
	}
	for lineNumber, line := range got {
		if width := lipgloss.Width(line); width > 5 {
			t.Fatalf("line %d width = %d, want <= 5: %q", lineNumber+1, width, line)
		}
	}
}

func TestWrapPreviewLinesTruncatesANSIStyledUnbrokenToken(t *testing.T) {
	got := wrapPreviewLines("\x1b[31m"+strings.Repeat("x", 200)+"\x1b[0m", 5, 20)
	if len(got) != 20 {
		t.Fatalf("expected 20 lines, got %#v", got)
	}
	if !strings.HasSuffix(ansi.Strip(got[len(got)-1]), "...") {
		t.Fatalf("expected truncation marker, got %#v", got)
	}
	for lineNumber, line := range got {
		if width := lipgloss.Width(line); width > 5 {
			t.Fatalf("line %d width = %d, want <= 5: %q", lineNumber+1, width, line)
		}
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
