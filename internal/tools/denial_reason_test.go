package tools

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
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

func TestFormatDenialError_SkipsLeadingBlankLines(t *testing.T) {
	// Leading blank lines must not silently drop the user's reason.
	got := formatDenialError("bash", "\n\n  actual reason\nmore")
	want := "user denied permission for tool 'bash'. Reason: actual reason"
	if got != want {
		t.Errorf("formatDenialError with leading blanks = %q, want %q", got, want)
	}
}

// withDenialReasonStdin replaces denialReasonStdin for the duration of the test.
//
// NOT parallel-safe: it mutates a package global without synchronization.
// Tests using this helper must not call t.Parallel().
func withDenialReasonStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	orig := denialReasonStdin
	denialReasonStdin = func() *bufio.Reader {
		return bufio.NewReader(strings.NewReader(input))
	}
	t.Cleanup(func() { denialReasonStdin = orig })
	fn()
}

func TestPromptDenialReason_EmptyAndEOF(t *testing.T) {
	// Both "\n" (Enter on empty line) and "" (immediate Ctrl-D / EOF)
	// are documented as yielding an empty reason string.
	cases := []struct {
		name  string
		input string
	}{
		{name: "EmptyInput", input: "\n"},
		{name: "EOFInput", input: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDenialReasonStdin(t, tc.input, func() {
				got := promptDenialReason(context.Background())
				if got != "" {
					t.Errorf("promptDenialReason with %s = %q, want \"\"", tc.name, got)
				}
			})
		})
	}
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

	// Record the call order so we can assert Start is invoked before Done.
	var (
		orderMu sync.Mutex
		order   []string
	)
	record := func(label string, counter *int32) func() {
		return func() {
			atomic.AddInt32(counter, 1)
			orderMu.Lock()
			order = append(order, label)
			orderMu.Unlock()
		}
	}

	origStart, origDone := onAskUserStart, onAskUserDone
	SetOnAskUserStart(record("start", &startCount))
	SetOnAskUserDone(record("done", &doneCount))
	t.Cleanup(func() {
		SetOnAskUserStart(origStart)
		SetOnAskUserDone(origDone)
	})

	withDenialReasonStdin(t, "test reason\n", func() {
		_ = promptDenialReason(context.Background())
	})

	if got := atomic.LoadInt32(&startCount); got != 1 {
		t.Errorf("NotifyAskUserStart called %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&doneCount); got != 1 {
		t.Errorf("NotifyAskUserDone called %d times, want 1", got)
	}

	// Verify ordering: Start must be recorded before Done.
	orderMu.Lock()
	defer orderMu.Unlock()
	var startIdx, doneIdx int = -1, -1
	for i, label := range order {
		switch label {
		case "start":
			if startIdx == -1 {
				startIdx = i
			}
		case "done":
			if doneIdx == -1 {
				doneIdx = i
			}
		}
	}
	if startIdx == -1 || doneIdx == -1 {
		t.Fatalf("expected both start and done to be recorded, got %v", order)
	}
	if startIdx >= doneIdx {
		t.Errorf("expected Start to be invoked before Done, got order %v", order)
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
	})
	// Close the writer *after* the prompt has been written so the read
	// side sees EOF. Doing this outside the helper callback also makes
	// the test robust if the callback panics.
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	buf, _ := io.ReadAll(r)
	got := string(buf)
	if !strings.Contains(got, "Reason (optional, enter to skip):") {
		t.Errorf("expected prompt to contain 'Reason (optional, enter to skip):', got %q", got)
	}
}
