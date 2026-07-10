package main

import (
	"strings"
	"testing"

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
