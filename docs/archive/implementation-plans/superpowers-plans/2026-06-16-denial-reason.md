# Denial Reason for Tool Permissions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a user denies a tool-call permission, optionally collect a free-text reason and append it to the error result returned to the LLM, so the agent can adapt.

**Architecture:** Two new helper functions in `internal/tools/tools.go` — `formatDenialError` (pure) and `promptDenialReason` (reads one line of stdin). The denial branch in `coreTool.Run` is modified to call `promptDenialReason` and use `formatDenialError` to build the result string. The TUI release/restore is handled by the existing `NotifyAskUserStart` / `NotifyAskUserDone` hooks. The reason is optional; empty reason preserves today's byte-identical error string.

**Tech Stack:** Go 1.26, `internal/tools` package, `bufio` for stdin reading, `NotifyAskUserStart` / `NotifyAskUserDone` / `StdinMu` / `SetAskUserActive` for terminal coordination, `fantasy.NewTextErrorResponse` for the LLM-facing result.

**Spec:** `docs/superpowers/specs/2026-06-16-denial-reason-design.md`

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/tools/tools.go` (modify) | Add `formatDenialError` and `promptDenialReason`. Modify the `!allowed` branch in `coreTool.Run` (currently at `tools.go:435-440`) to call them. |
| `internal/tools/denial_reason_test.go` (new) | Unit tests for both helpers. |

No other files change. The TUI, `ask_user`, audit hook, and session permission storage are all untouched.

---

## Task 1: Add `formatDenialError` helper + tests (TDD)

**Files:**
- Modify: `internal/tools/tools.go` (add helper after the `askUserActive` block, near the permission code at line ~208)
- Create: `internal/tools/denial_reason_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/tools/denial_reason_test.go`:

```go
package tools

import "testing"

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
	// Pure-whitespace input should be treated as "no reason" so the LLM
	// gets the original error string (no spurious "Reason: " suffix).
	got := formatDenialError("bash", "   \n  \n  ")
	want := "user denied permission for tool 'bash'"
	if got != want {
		t.Errorf("formatDenialError with whitespace-only = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestFormatDenialError -v`
Expected: FAIL with `formatDenialError undefined` (or similar build error).

- [ ] **Step 3: Add the `formatDenialError` implementation**

In `internal/tools/tools.go`, immediately after the `CheckToolPermission` function (after line 208, before the `ciEnvVars` var block on line 210), insert:

```go
// formatDenialError builds the error string returned to the LLM when the
// user denies a tool permission. When reason is empty, the result is
// byte-identical to the pre-feature format so existing agents see no change.
// When reason is non-empty, it is trimmed of surrounding whitespace, its
// first non-empty line is used, and the result is appended to the standard
// denial prefix.
func formatDenialError(toolName, reason string) string {
	cleaned := strings.TrimSpace(reason)
	if cleaned == "" {
		return fmt.Sprintf("user denied permission for tool '%s'", toolName)
	}
	// Take the first non-empty line.
	if idx := strings.IndexAny(cleaned, "\r\n"); idx >= 0 {
		cleaned = cleaned[:idx]
		cleaned = strings.TrimSpace(cleaned)
	}
	if cleaned == "" {
		return fmt.Sprintf("user denied permission for tool '%s'", toolName)
	}
	return fmt.Sprintf("user denied permission for tool '%s'. Reason: %s", toolName, cleaned)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run TestFormatDenialError -v`
Expected: PASS — all 5 sub-tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/tools.go internal/tools/denial_reason_test.go
git commit -m "feat(tools): add formatDenialError helper for permission denials"
```

---

## Task 2: Add `promptDenialReason` helper + tests (TDD)

**Files:**
- Modify: `internal/tools/tools.go` (add helper next to `formatDenialError`)
- Modify: `internal/tools/denial_reason_test.go` (add stdin-based tests)

The helper reads one line from stdin. To make it testable without touching real stdin, we use the same pattern as the existing `ask_user.go` / `path_consent.go` code: a package-level `os.Stdin` indirection. We will inject a `*bufio.Reader` via a test seam.

- [ ] **Step 1: Add the test seam (package-level reader)**

In `internal/tools/tools.go`, near the top of the file (just below the existing `askUserActive` var declaration on line 22), add:

```go
// denialReasonStdin is the reader used by promptDenialReason. It is a
// package-level variable so tests can inject a fake stdin.
var denialReasonStdin = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }

// denialReasonStderr is the writer used by promptDenialReason. Tests may
// redirect this to capture output.
var denialReasonStderr = os.Stderr
```

- [ ] **Step 2: Write the failing tests for `promptDenialReason`**

Append the following to `internal/tools/denial_reason_test.go`:

```go
import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

// silenceUnusedTimeKeepsImport guards against accidental import removal if a
// later refactor drops the only usage of `time` in this file.
var _ = time.Second
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestPromptDenialReason -v`
Expected: FAIL with `promptDenialReason undefined` (build error).

- [ ] **Step 4: Add the `promptDenialReason` implementation**

In `internal/tools/tools.go`, immediately after `formatDenialError`, insert:

```go
// promptDenialReason reads an optional one-line free-text reason from the
// user after they have denied a tool permission. The reason is returned to
// the LLM as part of the tool-error result. It is optional: an empty input
// (or a cancelled context) yields an empty string and the caller falls back
// to the standard "user denied" error string with no Reason suffix.
//
// The function is safe to call in TUI mode: it invokes NotifyAskUserStart
// to release the TUI altscreen (a no-op in CLI mode) and NotifyAskUserDone
// to restore it. It also serializes on StdinMu so it does not race with
// ask_user or the promptInjector.
func promptDenialReason(ctx context.Context) string {
	NotifyAskUserStart()

	StdinMu.Lock()
	defer StdinMu.Unlock()

	SetAskUserActive(true)
	defer SetAskUserActive(false)

	// Re-check the context after acquiring StdinMu: another goroutine may
	// have been waiting for the lock while the user (or the system)
	// cancelled the operation.
	if err := ctx.Err(); err != nil {
		return ""
	}

	reader := denialReasonStdin()
	fmt.Fprint(denialReasonStderr, "Reason (optional, enter to skip): ")

	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	// If the user pressed Ctrl-D (EOF) without typing, line will be "".
	// If they typed text without a trailing newline, we still get the text.
	// Trim trailing CR/LF.
	line = strings.TrimRight(line, "\r\n")
	return strings.TrimSpace(line)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run TestPromptDenialReason -v`
Expected: PASS — all 7 sub-tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/tools/tools.go internal/tools/denial_reason_test.go
git commit -m "feat(tools): add promptDenialReason helper for optional denial reason"
```

---

## Task 3: Wire the helpers into `coreTool.Run`

**Files:**
- Modify: `internal/tools/tools.go` (modify the `!allowed` denial branch around line 435-440)

- [ ] **Step 1: Read the current state of the denial branch**

Read `internal/tools/tools.go` lines 380-441. Confirm the structure:

```go
if !allowed {
    if askUser {
        // ... four-choice prompt logic ...
        if !allowed {
            return fantasy.NewTextErrorResponse(fmt.Sprintf("user denied permission for tool '%s'", t.info.Name)), nil
        }
    } else {
        return fantasy.NewTextErrorResponse(fmt.Sprintf("tool '%s' is not permitted. Add '%s' to tools.allowed in team.yaml to enable.", t.info.Name, t.info.Name)), nil
    }
}
```

- [ ] **Step 2: Replace the `user denied permission` return**

The current code is on line 436:

```go
return fantasy.NewTextErrorResponse(fmt.Sprintf("user denied permission for tool '%s'", t.info.Name)), nil
```

Replace it with:

```go
// Ask the user for an optional reason so the agent can adapt
// (pick a different tool, change parameters, ask a follow-up).
reason := promptDenialReason(ctx)
return fantasy.NewTextErrorResponse(formatDenialError(t.info.Name, reason)), nil
```

Do **not** change the surrounding `if !allowed` / `if !askUser` branches — only the inner denial return.

- [ ] **Step 3: Verify the package still builds**

Run: `go build ./...`
Expected: clean build, no errors.

- [ ] **Step 4: Run the full `tools` test suite**

Run: `go test ./internal/tools/ -v`
Expected: all tests pass, including the new ones from Tasks 1 and 2 and the pre-existing `TestCheckToolPermission_*` / `TestSetOnAskUserStart_NotifyAskUserStart` / `TestSetOnAskUserDone_NotifyAskUserDone` tests.

- [ ] **Step 5: Run `go vet`**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add internal/tools/tools.go
git commit -m "feat(tools): include optional reason in denial error returned to LLM"
```

---

## Task 4: Final regression sweep

**Files:** none modified

- [ ] **Step 1: Run the full repository test suite**

Run: `go test ./...`
Expected: all packages pass. The change is scoped to `internal/tools` and does not affect any other package's public API.

- [ ] **Step 2: Run `go vet` across the repo**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Run `go build` to confirm the binary builds**

Run: `go build ./cmd/hufu`
Expected: `hufu` binary produced with no errors.

- [ ] **Step 4: Manual smoke test (optional but recommended)**

In a scratch workspace:

```bash
# Build and run with a simple team that uses a medium-risk tool.
# Trigger a permission prompt and pick "n", then provide a reason.
# Observe that the next LLM response references the reason.
go run ./cmd/hufu --agent-team <your-team> --workspace /tmp/hufu-deny-test "..."
```

For the TUI variant:

```bash
go run ./cmd/hufu --tui --agent-team <your-team> --workspace /tmp/hufu-deny-test "..."
```

Confirm the TUI releases the altscreen for the reason prompt and restores cleanly afterward.

- [ ] **Step 5: Confirm git log shows the feature commits**

Run: `git log --oneline -5`
Expected: three new commits, in order:
1. `feat(tools): add formatDenialError helper for permission denials`
2. `feat(tools): add promptDenialReason helper for optional denial reason`
3. `feat(tools): include optional reason in denial error returned to LLM`

---

## Self-Review

**Spec coverage:**
- ✅ Goal — optional free-text reason appended to error → `formatDenialError` (Task 1) + `promptDenialReason` (Task 2) + `coreTool.Run` wiring (Task 3).
- ✅ CLI flow → handled by `promptDenialReason` reading stdin after the four-choice prompt.
- ✅ TUI flow → `NotifyAskUserStart` releases the altscreen (existing hook) before the read.
- ✅ Empty reason → byte-identical string via `formatDenialError(_, "")` → falls back to original format.
- ✅ Whitespace trimmed → covered by `TestFormatDenialError_TrimsWhitespace` and `TestPromptDenialReason_TrimsWhitespace`.
- ✅ First non-empty line only → covered by `TestFormatDenialError_FirstLineOnly` and `TestFormatDenialError_WhitespaceBecomesEmpty`.
- ✅ Ctrl-C / context cancellation → empty string → covered by `TestPromptDenialReason_CancelledContext`.
- ✅ Hooks invoked once → covered by `TestPromptDenialReason_InvokesStartDoneHooks`.
- ✅ Stderr prompt text → covered by `TestPromptDenialReason_WritesPromptToStderr`.

**Placeholder scan:** No TBDs, no "implement later", all code blocks complete.

**Type consistency:** `promptDenialReason(ctx context.Context) string` and `formatDenialError(toolName, reason string) string` are used identically across all three tasks.

**No spec requirement is missing a task.**
