# Denial Reason for Tool Permissions — Design Specification

**Date**: 2026-06-16
**Author**: hufu development team
**Status**: Draft (pending review)

---

## Executive Summary

When the user denies a tool-call permission (one-shot "No" or session-wide "Always Deny"), hufu today returns a bare error string to the LLM: `user denied permission for tool 'X'`. The agent has no idea *why* the user refused and tends to either retry the same call or stall. This spec adds an **optional, follow-up free-text reason** to the existing permission prompt. The reason is appended to the error result sent back to the LLM, so the agent can adapt (pick a different tool, change parameters, ask a follow-up, give up gracefully).

The change is scoped to the permission-denial path. No new TUI overlays, no new tool API, no new permission options, no new audit format.

---

## Current State Analysis

### Existing permission flow (`internal/tools/tools.go:352-441`)

`coreTool.Run` calls `CheckToolPermission(ctx, toolName)`. When the tool is not in the allowlist but the environment is interactive, the function returns `askUser=true`, and `coreTool.Run` then prompts the user with four options:

- `(y)` Yes
- `(n)` No
- `(ay)` Always Allow
- `(an)` Always Deny

The prompt is delivered via one of two paths:

1. **TUI** — `tools.TryAskUserTUI` (`tools.go:87`) which the TUI registers in `cmd/hufu/display.go:67-101`. The TUI presents a single-choice dialog with the four options. Result is a JSON `askResponseType` with the chosen value.
2. **CLI fallback** — when the TUI is not active, `coreTool.Run` prints the prompt to stderr and reads stdin directly (`tools.go:413-433`).

When the user picks "No" or "Always Deny", `coreTool.Run` returns `fantasy.NewTextErrorResponse(fmt.Sprintf("user denied permission for tool '%s'", t.info.Name))` (`tools.go:436`). This string becomes the tool's `result` and is fed back to the LLM. The LLM has no information about *why* the user refused.

### Terminal release pattern

Before reading stdin in CLI mode, `coreTool.Run` calls `StdinMu.Lock()` to serialize with `ask_user` and the `promptInjector` (Ctrl+Z/SIGUSR1 prompt injection). In TUI mode, the existing `SetOnAskUserStart` / `SetOnAskUserDone` hooks (`tools.go:42-64`) release and restore the TUI's altscreen via `tea.Program.ReleaseTerminal` / `RestoreTerminal` (registered in `cmd/hufu/display.go:104-134`). This same pattern is reused for the new reason input.

### `ask_user` is unrelated

`ask_user` is an agent-facing clarification tool. The permission prompt is a separate, system-facing flow that lives in `coreTool.Run`. The new reason input stays in the permission flow and does not modify `ask_user`.

---

## Design Goals

1. **Optional, low-friction** — the user can press Enter to skip the reason. Existing users see no behavior change unless they choose to type.
2. **Context for the LLM** — the reason must reach the LLM as part of the tool-error result so the agent can adapt.
3. **No regression** — when the reason is empty, the result string is byte-identical to today.
4. **Reuse existing primitives** — `NotifyAskUserStart` / `NotifyAskUserDone` and `StdinMu` already exist; we use them as-is.
5. **No TUI overlay changes** — the TUI does not need a new mode. The reason is read in the altscreen-released window.

---

## Non-Goals

- No new permission options (no "Yes with constraint" etc.).
- No new TUI messages, modes, or overlays.
- No changes to `CheckToolPermission`, the `ask_user` tool, or the audit-log format.
- No reason input for the "Yes" / "Always Allow" paths.
- No structured result payload; the reason is folded into the existing error string.

---

## Proposed Design

### User-visible flow (CLI mode)

```
PERMISSION: Agent 'developer' wants to use tool 'bash'. Allow?
  (y) Yes  (n) No  (ay) Always Yes  (an) Always No
  Choice [n]: n
Reason (optional, enter to skip): please don't delete files
```

The reason is read on the next line. Empty input (just Enter) preserves today's behavior exactly.

### User-visible flow (TUI mode)

When the TUI is active, the four-option dialog renders as today. After the user picks "No" or "Always Deny", the TUI releases its altscreen via the existing `SetOnAskUserStart` hook, the reason prompt is shown in the normal scrollback (`Reason (optional, enter to skip):`), the user types, Enter is pressed, and the TUI is restored via `SetOnAskUserDone`. The TUI itself does not render a textinput — the read happens in the released-terminal window, the same way the existing permission prompt handles "y/n" input in TUI mode today.

### Final error string format

Two cases, decided by whether the reason is empty:

| Reason input | Error returned to LLM |
|---|---|
| Empty (or skipped via Ctrl-C / EOF) | `user denied permission for tool 'bash'` (byte-identical to today) |
| Non-empty (e.g. `please don't delete files`) | `user denied permission for tool 'bash'. Reason: please don't delete files` |

The reason is trimmed of leading/trailing whitespace and, if it contains newlines, only the first non-empty line is used.

### Data flow

1. `coreTool.Run` (`tools.go:382-441`) — when `!allowed && askUser`:
   - Existing 4-choice prompt (TUI or CLI), unchanged.
   - On `n` / `an` answer, before returning the error, call a new helper:
     ```go
     reason := promptDenialReason(ctx)
     return fantasy.NewTextErrorResponse(formatDenialError(toolName, reason))
     ```
2. `promptDenialReason(ctx) string`:
   - Calls `NotifyAskUserStart()` (releases TUI altscreen if active, no-op in CLI mode).
   - `StdinMu.Lock()`; defer `StdinMu.Unlock()`.
   - `SetAskUserActive(true)`; defer `SetAskUserActive(false)`.
   - Prints `Reason (optional, enter to skip): ` to stderr.
   - Reads one line from stdin (a `bufio.NewReader(os.Stdin)`).
   - If `ctx.Err() != nil` at any point, returns `""` and treats it as a denial with no reason.
   - Trims and returns the line.
3. `formatDenialError(toolName, reason string) string`:
   - If `reason == ""` → `fmt.Sprintf("user denied permission for tool '%s'", toolName)`.
   - Otherwise → `fmt.Sprintf("user denied permission for tool '%s'. Reason: %s", toolName, reason)`.

### Edge cases

| Case | Behavior |
|---|---|
| User denies, types nothing, presses Enter | `reason == ""` → identical string to today |
| User denies, types `please don't delete files`, presses Enter | `reason == "please don't delete files"` → new format with `Reason: …` |
| User denies, types `  text with spaces  ` | Trimmed to `text with spaces` |
| User denies, types a multi-line reason (pasted block) | Only the first non-empty line is used; rest discarded (LLMs handle short context better) |
| User presses Ctrl-C during the reason prompt | Treated as "denied, no reason" (empty string). The session-level `Always Deny` decision is still recorded. `coreTool.Run` returns the standard error. |
| Context is cancelled during the reason prompt | Same as Ctrl-C. |
| Non-TTY / CI environment | `isInteractiveEnvironment()` returns false → `CheckToolPermission` never returns `askUser=true` → reason prompt is unreachable. |
| `Yes` or `Always Allow` chosen | No reason prompt. Behavior unchanged. |
| Permission callback context key missing | Helper is best-effort; the TUI release/restore hooks are nil-safe (existing `NotifyAskUserStart` checks `if onAskUserStart != nil`). |

### Files changed

| File | Change |
|---|---|
| `internal/tools/tools.go` | Add `promptDenialReason`, `formatDenialError`. Modify the denial branch in `coreTool.Run` to call them. |
| `internal/tools/denial_reason_test.go` (new) | Unit tests for the helper and the formatter. |

No other files need to change. The TUI, `ask_user`, the audit hook, the `SetOnAskUserStart`/`Done` infrastructure, the session permission storage, and the `ask_user` TUI dialog are all untouched.

### Architecture & isolation

The new helpers are **stateless** and **self-contained**:

- `promptDenialReason` depends only on `os.Stdin`, `os.Stderr`, the existing `NotifyAskUserStart`/`Done` hooks, and `ctx.Err()`. It is a pure read; no globals mutated.
- `formatDenialError` is a pure function.

They live in `internal/tools/tools.go` next to the existing permission code, keeping the boundary between the "permission gate" and the rest of the package clear. They are not added to `coreTool` because they are not tools — they are helpers used by `coreTool.Run`.

---

## Testing Strategy

### Unit tests (new file `internal/tools/denial_reason_test.go`)

1. **`TestFormatDenialError`**:
   - Empty reason → returns `"user denied permission for tool 'bash'"` (byte-identical to today).
   - Reason `"x"` → returns `"user denied permission for tool 'bash'. Reason: x"`.
   - Reason with leading/trailing whitespace `"  y  "` → returns `"...Reason: y"`.
   - Reason containing newlines `"a\nb\nc"` → returns `"...Reason: a"` (first non-empty line).
2. **`TestPromptDenialReason_EmptyInput`**:
   - Stdin returns `""` (or just `"\n"`) → helper returns `""`.
3. **`TestPromptDenialReason_WithReason`**:
   - Stdin returns `"please don't delete files\n"` → helper returns `"please don't delete files"`.
4. **`TestPromptDenialReason_CancelledContext`**:
   - Context cancelled before the read → helper returns `""`.
5. **`TestPromptDenialReason_InvokesStartDoneHooks`**:
   - Register a counter on `SetOnAskUserStart` and `SetOnAskUserDone`. After the helper runs, both are called exactly once each.

### Existing tests (no change expected)

- `TestCheckToolPermission_*` — `CheckToolPermission` is unchanged, so all current tests pass.
- `TestSetOnAskUserStart_NotifyAskUserStart`, `TestSetOnAskUserDone_NotifyAskUserDone` — the hooks are unchanged.

### Manual / TUI verification

- CLI: `hufu --agent-team …` then trigger a medium-risk tool, deny with reason, confirm the LLM receives the reason in the next round's tool result.
- TUI: `hufu --tui --agent-team …` then trigger a medium-risk tool, deny with reason, confirm the TUI releases the altscreen for the reason prompt and restores cleanly afterward.
- Empty reason: confirm the tool result string is byte-identical to pre-change behavior (regression guard).

---

## Rollout

1. Land the helper + formatter + tests in one PR.
2. No new CLI flags, no config changes, no migration.
3. No deprecation: existing user behavior is preserved when no reason is supplied.

---

## Open questions

None. All clarifying questions resolved during brainstorming.
