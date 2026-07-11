package main

import (
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

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
	got := wrapPreviewLines("go run ./cmd/pilot vm-target exec --name ipa-ha-client -- cat /etc/ssh/sshd_config", 120, 4)
	if len(got) != 1 {
		t.Fatalf("expected one line, got %#v", got)
	}
	if strings.Contains(got[0], "...") {
		t.Fatalf("did not expect ellipsis in %q", got[0])
	}
}

func TestWrapPreviewLinesWrapsWithoutPrematureEllipsis(t *testing.T) {
	got := wrapPreviewLines("go run ./cmd/pilot vm-target exec --name ipa-ha-client -- cat /etc/ssh/sshd_config", 36, 4)
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
