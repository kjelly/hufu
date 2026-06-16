package tools

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFormatDenialError_EmptyReason(t *testing.T) {
	got := formatDenialError("bash", "")
	want := "user denied permission for tool 'bash'"
	if got != want {
		t.Errorf("formatDenialError(\"bash\", \"\") = %q, want %q", got, want)
	}
}

func TestFormatDenialError_WithReason(t *testing.T) {
	got := formatDenialError("bash", "please don't delete files")
	want := "user denied permission for tool 'bash'. Reason: please don't delete files"
	if got != want {
		t.Errorf("formatDenialError = %q, want %q", got, want)
	}
}

func TestFormatDenialError_TrimsWhitespace(t *testing.T) {
	got := formatDenialError("bash", "  trimmed reason  ")
	want := "user denied permission for tool 'bash'. Reason: trimmed reason"
	if got != want {
		t.Errorf("formatDenialError with whitespace = %q, want %q", got, want)
	}
}

func TestFormatDenialError_FirstLineOnly(t *testing.T) {
	got := formatDenialError("bash", "first line\nsecond line\nthird")
	want := "user denied permission for tool 'bash'. Reason: first line"
	if got != want {
		t.Errorf("formatDenialError with newlines = %q, want %q", got, want)
	}
}

func TestFormatDenialError_WhitespaceBecomesEmpty(t *testing.T) {
	got := formatDenialError("bash", "   \n  \n  ")
	want := "user denied permission for tool 'bash'"
	if got != want {
		t.Errorf("formatDenialError with whitespace-only = %q, want %q", got, want)
	}
}

// withDenialReasonStdin replaces denialReasonStdin for the duration of the test.
func withDenialReasonStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	orig := denialReasonStdin
	denialReasonStdin = func() *bufio.Reader {
		return bufio.NewReader(strings.NewReader(input))
	}
	t.Cleanup(func() { denialReasonStdin = orig })
	fn()
}

func TestPromptDenialReason_EmptyInput(t *testing.T) {
	withDenialReasonStdin(t, "\n", func() {
		got := promptDenialReason(context.Background())
		if got != "" {
			t.Errorf("promptDenialReason with empty input = %q, want \"\"", got)
		}
	})
}

func TestPromptDenialReason_EOFInput(t *testing.T) {
	// Reader returns io.EOF immediately (no newline, empty string).
	withDenialReasonStdin(t, "", func() {
		got := promptDenialReason(context.Background())
		if got != "" {
			t.Errorf("promptDenialReason with EOF input = %q, want \"\"", got)
		}
	})
}

func TestPromptDenialReason_WithReason(t *testing.T) {
	withDenialReasonStdin(t, "please don't delete files\n", func() {
		got := promptDenialReason(context.Background())
		want := "please don't delete files"
		if got != want {
			t.Errorf("promptDenialReason = %q, want %q", got, want)
		}
	})
}

func TestPromptDenialReason_TrimsWhitespace(t *testing.T) {
	withDenialReasonStdin(t, "  trimmed  \n", func() {
		got := promptDenialReason(context.Background())
		want := "trimmed"
		if got != want {
			t.Errorf("promptDenialReason = %q, want %q", got, want)
		}
	})
}

func TestPromptDenialReason_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before the read.
	withDenialReasonStdin(t, "this should not be used\n", func() {
		got := promptDenialReason(ctx)
		if got != "" {
			t.Errorf("promptDenialReason with cancelled ctx = %q, want \"\"", got)
		}
	})
}

func TestPromptDenialReason_InvokesStartDoneHooks(t *testing.T) {
	var startCount, doneCount int32

	origStart, origDone := onAskUserStart, onAskUserDone
	SetOnAskUserStart(func() { atomic.AddInt32(&startCount, 1) })
	SetOnAskUserDone(func() { atomic.AddInt32(&doneCount, 1) })
	t.Cleanup(func() {
		SetOnAskUserStart(origStart)
		SetOnAskUserDone(origDone)
	})

	withDenialReasonStdin(t, "test reason\n", func() {
		_ = promptDenialReason(context.Background())
	})

	if atomic.LoadInt32(&startCount) != 1 {
		t.Errorf("NotifyAskUserStart called %d times, want 1", startCount)
	}
	if atomic.LoadInt32(&doneCount) != 1 {
		t.Errorf("NotifyAskUserDone called %d times, want 1", doneCount)
	}
}

func TestPromptDenialReason_WritesPromptToStderr(t *testing.T) {
	origStderr := denialReasonStderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer r.Close()
	denialReasonStderr = w
	t.Cleanup(func() { denialReasonStderr = origStderr })

	withDenialReasonStdin(t, "ok\n", func() {
		_ = promptDenialReason(context.Background())
		// Close the writer so the read side sees EOF.
		if err := w.Close(); err != nil {
			t.Fatalf("close pipe writer: %v", err)
		}
		buf, _ := io.ReadAll(r)
		got := string(buf)
		if !strings.Contains(got, "Reason (optional, enter to skip):") {
			t.Errorf("expected prompt to contain 'Reason (optional, enter to skip):', got %q", got)
		}
	})
}
