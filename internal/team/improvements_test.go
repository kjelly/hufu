package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
)

// ── Deliverable verification ──────────────────────────────────────────────────

func newVerifyCoordinator(t *testing.T, projectDir string) *Coordinator {
	t.Helper()
	return &Coordinator{
		session:    &TeamSession{Config: agent.TeamConfig{Name: "test", Timeout: 30}},
		projectDir: projectDir,
	}
}

func TestVerifyTaskDeliverable_EmptyCommand(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	if err := c.verifyTaskDeliverable(context.Background(), nil, ""); err != nil {
		t.Errorf("empty verify command should be a no-op, got %v", err)
	}
}

func TestVerifyTaskDeliverable_Success(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newVerifyCoordinator(t, dir)
	if err := c.verifyTaskDeliverable(context.Background(), nil, "test -f report.md"); err != nil {
		t.Errorf("expected success when deliverable exists, got %v", err)
	}
}

func TestVerifyTaskDeliverable_FailureWhenMissing(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	err := c.verifyTaskDeliverable(context.Background(), nil, "test -f does-not-exist.md")
	if err == nil {
		t.Fatal("expected error when deliverable is missing")
	}
}

func TestVerifyTaskDeliverable_FailureIncludesOutput(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	err := c.verifyTaskDeliverable(context.Background(), nil, "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include command output, got %q", err.Error())
	}
}

// ── Repeated-failure detection ────────────────────────────────────────────────

func TestSameFailure(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"connection refused", "connection refused", true},
		{"attempt 1 failed: connection refused", "attempt 2 failed: connection refused", true},
		{"Connection Refused", "connection refused", true},
		{"timeout", "connection refused", false},
		{"", "", false},
		{"", "x", false},
	}
	for _, tt := range tests {
		if got := sameFailure(tt.a, tt.b); got != tt.want {
			t.Errorf("sameFailure(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// ── Local failure hints ───────────────────────────────────────────────────────

func TestLocalFailureHint(t *testing.T) {
	tests := []struct {
		in       string
		contains string
	}{
		{"deliverable verification failed (command \"x\")", "verification check failed"},
		{"context deadline exceeded", "timed out"},
		{"open foo: no such file or directory", "not found"},
		{"permission denied", "permission"},
		{"step count limit reached", "out of steps"},
		{"duplicate task detected", "already-completed"},
		{"some unknown explosion", "Change your approach"},
	}
	for _, tt := range tests {
		got := localFailureHint(tt.in)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("localFailureHint(%q) = %q, want substring %q", tt.in, got, tt.contains)
		}
	}
}

// ── Worker auxiliary-context budget ───────────────────────────────────────────

func TestAssembleContextWithinBudget(t *testing.T) {
	a := strings.Repeat("A", 100)
	b := strings.Repeat("B", 100)
	cc := strings.Repeat("C", 100)

	// Budget fits all three.
	got := assembleContextWithinBudget([]string{a, b, cc}, 5000)
	for _, p := range []string{a, b, cc} {
		if !strings.Contains(got, p) {
			t.Errorf("expected all parts within ample budget")
		}
	}
	if !strings.HasPrefix(got, "\n\n") {
		t.Errorf("non-empty result must be prefixed with blank line for appending")
	}

	// Budget fits only the first two; lowest-priority dropped.
	got = assembleContextWithinBudget([]string{a, b, cc}, 250)
	if !strings.Contains(got, a) || !strings.Contains(got, b) {
		t.Errorf("higher-priority parts should be kept")
	}
	if strings.Contains(got, cc) {
		t.Errorf("lowest-priority part should be dropped when over budget")
	}

	// Zero budget yields nothing.
	if got := assembleContextWithinBudget([]string{a}, 0); got != "" {
		t.Errorf("zero budget should yield empty, got %q", got)
	}

	// Empty parts ignored.
	if got := assembleContextWithinBudget([]string{"", ""}, 100); got != "" {
		t.Errorf("all-empty parts should yield empty, got %q", got)
	}
}

// ── Conversation-history head preservation ────────────────────────────────────

func msgWith(text string) fantasy.Message { return fantasy.NewUserMessage(text) }

func TestTrimHistoryPreservingHead(t *testing.T) {
	msgs := make([]fantasy.Message, 20)
	for i := range msgs {
		msgs[i] = msgWith(string(rune('a' + i)))
	}

	trimmed := trimHistoryPreservingHead(msgs, 10)
	if len(trimmed) != 10 {
		t.Fatalf("expected length 10, got %d", len(trimmed))
	}
	// First message (the goal/setup) must be preserved.
	if firstText(trimmed[0]) != firstText(msgs[0]) {
		t.Errorf("head message not preserved: got %q want %q", firstText(trimmed[0]), firstText(msgs[0]))
	}
	// Last message must be the most recent.
	if firstText(trimmed[len(trimmed)-1]) != firstText(msgs[len(msgs)-1]) {
		t.Errorf("tail message not preserved")
	}

	// No-op when already within max.
	if got := trimHistoryPreservingHead(msgs[:5], 10); len(got) != 5 {
		t.Errorf("within-max should be unchanged, got len %d", len(got))
	}

	// Non-positive max yields nil.
	if got := trimHistoryPreservingHead(msgs, 0); got != nil {
		t.Errorf("max<=0 should yield nil")
	}
}

func firstText(m fantasy.Message) string {
	for _, part := range m.Content {
		if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			return txt.Text
		}
	}
	return ""
}
