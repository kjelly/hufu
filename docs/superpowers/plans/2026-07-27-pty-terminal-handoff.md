# PTY Terminal Sessions with Human Handoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in PTY terminal sessions that an agent can operate and a local user can exclusively take over from the hufu TUI or `hufu terminal attach`.

**Architecture:** Extend `internal/team.TerminalSessionManager` with `pipe|pty` modes, exclusive controller leases, bounded normalized screens, and an in-process Unix-socket broker. Keep the PTY master in the owning hufu process; attach clients communicate over the broker. The coordinator pauses the owner task for the duration of a user lease.

**Tech Stack:** Go 1.26.2, `golang.org/x/sys/unix`, Cobra, Bubble Tea, existing terminal-session and event-store infrastructure.

## Global Constraints

- Preserve all existing `bash` behavior; PTY belongs only to the stateful `terminal` tool.
- PTY creation is explicit (`pty:true`) and gated by `--enable-pty-terminal`.
- Linux and macOS only; unsupported platforms fail explicitly.
- The broker dies with its owning hufu process; recovery marks the session `unknown` and never retries it automatically.
- A terminal session has exactly one controller: `agent`, `user`, or `none`.
- User control pauses the owner task and blocks agent model/tool progress and terminal input.
- Raw terminal bytes remain in workspace artifacts; model context receives only a bounded normalized screen snapshot.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/team/terminal_session.go` | Persisted terminal state, mode/controller leases, task gates. |
| `internal/team/terminal_pty_unix.go` | Unix PTY start, output reader, resize, screen capture. |
| `internal/team/terminal_pty_stub.go` | Explicit unsupported-platform error. |
| `internal/team/terminal_broker.go` | In-process local socket server and lease authority. |
| `internal/team/terminal_broker_client.go` | Attach client protocol used by CLI/TUI. |
| `internal/team/coordinator_tools_terminal.go` | PTY start/resize/signal tool API. |
| `internal/team/coordinator*.go` | Pause/resume and handoff context. |
| `cmd/hufu/terminalcmd.go` | `terminal list`, `attach`, and `detach`. |
| `cmd/hufu/root.go`, `cmd/hufu/options.go` | Feature flag. |
| `internal/tui/tui.go`, `cmd/hufu/display.go` | Detail-view takeover and I/O forwarding. |

### Task 1: Persist terminal mode and exclusive controller leases

**Files:**
- Modify: `internal/team/terminal_session.go`
- Test: `internal/team/terminal_session_test.go`

**Produces:** `TerminalMode`, `TerminalController`, `TerminalLease`, `AcquireUserLease`, `ReleaseUserLease`, and agent-write rejection while a user controls the session.

- [ ] **Step 1: Write failing tests**

```go
func TestTerminalSessionLegacyRecordDefaultsToPipe(t *testing.T) {}
func TestTerminalSessionUserLeaseBlocksAgentWrite(t *testing.T) {}
func TestTerminalSessionStaleLeaseCannotReleaseNewLease(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/team -run 'TestTerminalSession(LegacyRecordDefaultsToPipe|UserLeaseBlocksAgentWrite|StaleLeaseCannotReleaseNewLease)' -count=1`

Expected: FAIL because the types and methods do not exist.

- [ ] **Step 3: Implement the smallest state model**

Add exact values `pipe`, `pty`, `none`, `agent`, and `user`; normalize missing persisted mode to `pipe`. Add `Mode`, `Controller`, `LeaseID`, `Rows`, `Cols`, and `AttachedAt` to `TerminalSession`. Add `Mode`, `Rows`, `Cols` to `TerminalStartRequest`. Guard all lease transitions under the manager mutex and emit `terminal_session_taken_over` / `terminal_session_released` events.

- [ ] **Step 4: Enforce the controller on writes**

Keep the existing task-owner check. Then reject agent writes with `terminal session "<id>" is controlled by a user` while a user lease is active.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/team -run TestTerminal -count=1`

Expected: PASS.

```bash
git add internal/team/terminal_session.go internal/team/terminal_session_test.go
git commit -m "feat(team): add terminal session control leases"
```

### Task 2: Add Unix PTY sessions and normalized screens

**Files:**
- Create: `internal/team/terminal_pty_unix.go`
- Create: `internal/team/terminal_pty_stub.go`
- Modify: `internal/team/terminal_session.go`
- Test: `internal/team/terminal_pty_test.go`

**Consumes:** Task 1 mode/session fields.

**Produces:** Real PTY child processes, `Resize`, and `TerminalReadResult.Screen`.

- [ ] **Step 1: Write failing PTY tests**

```go
func TestPTYSessionReportsTTYAndAcceptsInput(t *testing.T) {}
func TestPTYSessionResizeChangesChildSize(t *testing.T) {}
func TestPTYReadReturnsNormalizedBoundedScreen(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/team -run 'TestPTY(SessionReportsTTYAndAcceptsInput|SessionResizeChangesChildSize|ReadReturnsNormalizedBoundedScreen)' -count=1`

Expected: FAIL because PTY mode is not implemented.

- [ ] **Step 3: Implement PTY creation under Unix build tags**

Use `golang.org/x/sys/unix` to open `/dev/ptmx`, unlock/query its slave, set `Winsize`, and start the child with slave as stdin/stdout/stderr. Set `Setsid`, `Setctty`, `Ctty:0`, and retain existing process-group cleanup. Keep the current pipe branch unchanged. The non-Unix stub returns `PTY terminal sessions are supported only on linux and darwin`.

- [ ] **Step 4: Implement read, screen, and resize**

One goroutine reads the master, appends raw output to the terminal artifact, and updates a mutex-protected bounded ANSI-normalized screen. Persist the latest normalized screen under `logs/terminal-screens/`. `Resize(id, rows, cols)` validates positive dimensions, calls `TIOCSWINSZ`, and sends `SIGWINCH`.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/team -run 'Test(PTY|Terminal)' -race -count=1`

Expected: PASS.

```bash
git add internal/team/terminal_session.go internal/team/terminal_pty_unix.go internal/team/terminal_pty_stub.go internal/team/terminal_pty_test.go
git commit -m "feat(team): run terminal sessions in PTYs"
```

### Task 3: Build the in-process authenticated broker

**Files:**
- Create: `internal/team/terminal_broker.go`
- Create: `internal/team/terminal_broker_client.go`
- Modify: `internal/team/terminal_session.go`
- Test: `internal/team/terminal_broker_test.go`

**Consumes:** Task 1 lease state and Task 2 PTY master.

**Produces:** `StartTerminalBroker`, attach/detach/resize client API, and streamed local I/O.

- [ ] **Step 1: Write failing broker tests**

```go
func TestTerminalBrokerAttachStreamsInputAndOutput(t *testing.T) {}
func TestTerminalBrokerRejectsConcurrentAttach(t *testing.T) {}
func TestTerminalBrokerEOFLeavesOwnerTaskPaused(t *testing.T) {}
func TestTerminalBrokerRejectsDifferentUID(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/team -run TestTerminalBroker -count=1`

Expected: FAIL because broker types do not exist.

- [ ] **Step 3: Implement socket/protocol/authorization**

Listen at `<workspace>/logs/terminal-broker.sock`; parent directory is `0700`, socket is `0600`, and a stale path may be removed only after verifying it is a socket owned by the current UID. Implement length-prefixed JSON control frames and binary data frames for `attach`, `write`, `resize`, `detach`, and `status`. On Linux verify peers with `SO_PEERCRED`; provide a Darwin equivalent behind build tags and fail closed when verification is unavailable. Make the credential reader injectable for tests.

- [ ] **Step 4: Bind broker events to manager lease callbacks**

Attach atomically acquires a user lease, sends an initial normalized screen, then forwards bytes. Normal detach releases and invokes the resume callback. Socket EOF revokes the lease but invokes only the pause callback, leaving the task paused. A concurrent attach gets a lease-conflict error.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/team -run 'Test(PTY|TerminalBroker|TerminalSession)' -race -count=1`

Expected: PASS.

```bash
git add internal/team/terminal_broker.go internal/team/terminal_broker_client.go internal/team/terminal_broker_test.go internal/team/terminal_session.go
git commit -m "feat(team): broker local PTY attachments"
```

### Task 4: Wire terminal tool and coordinator pause/resume

**Files:**
- Modify: `internal/team/coordinator_tools_terminal.go`
- Modify: `internal/team/coordinator.go`
- Modify: `internal/team/coordinator_run.go`
- Modify: `internal/team/coordinator_task_run.go`
- Test: `internal/team/coordinator_tools_test.go`
- Test: `internal/team/terminal_session_test.go`

**Consumes:** Tasks 1–3.

**Produces:** `pty`, `rows`, `cols`, `resize`, `signal` terminal API and owner-task handoff.

- [ ] **Step 1: Write failing tool/coordinator tests**

```go
func TestTerminalToolStartsPTYOnlyWhenFeatureEnabled(t *testing.T) {}
func TestTerminalToolResizeRequiresTerminalPermission(t *testing.T) {}
func TestUserTakeoverPausesAndReleaseResumesOwnerTodo(t *testing.T) {}
func TestCoordinatorFinishRejectsActiveUserLease(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/team -run 'Test(TerminalTool|UserTakeover|CoordinatorFinishRejectsActiveUserLease)' -count=1`

Expected: FAIL because API/feature gate/handoff hooks do not exist.

- [ ] **Step 3: Extend terminal tool API**

Add tool args `pty`, `rows`, `cols`, and `signal`. Add actions `resize` and `signal`, permission names `terminal_resize` and `terminal_signal`, and allow only `INT`, `TERM`, `HUP`. `pty:true` returns `PTY terminal feature is disabled` unless the root flag is enabled.

- [ ] **Step 4: Implement owner task pause and release**

Track a cancel function per executing todo. On user lease: cancel that task's model context, set `TaskPaused` with detail `user takeover`, emit `todos_updated`, and persist a handoff event. On release: save optional user summary and bounded screen, set `TaskInProgress`, and arrange the next continuation prompt to include the handoff. Do not allow a new agent tool call while that todo has a user lease.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/team -count=1`

Expected: PASS.

```bash
git add internal/team/coordinator.go internal/team/coordinator_run.go internal/team/coordinator_task_run.go internal/team/coordinator_tools_terminal.go internal/team/coordinator_tools_test.go internal/team/terminal_session_test.go
git commit -m "feat(team): pause agent tasks for PTY takeover"
```

### Task 5: Add CLI feature flag and external attach commands

**Files:**
- Create: `cmd/hufu/terminalcmd.go`
- Create: `cmd/hufu/terminalcmd_test.go`
- Modify: `cmd/hufu/root.go`
- Modify: `cmd/hufu/options.go`
- Modify: `cmd/hufu/run.go`

**Consumes:** Task 3 broker client and Task 4 feature gate.

**Produces:** `--enable-pty-terminal`, `hufu terminal list`, `attach`, and `detach`.

- [ ] **Step 1: Write failing command tests**

```go
func TestTerminalAttachRequiresTTY(t *testing.T) {}
func TestTerminalAttachForwardsAndCtrlBracketDetaches(t *testing.T) {}
func TestTerminalCommandsResolveWorkspace(t *testing.T) {}
func TestUnattendedRejectsTerminalAttach(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./cmd/hufu -run 'TestTerminal(Attach|Commands)|TestUnattendedRejectsTerminalAttach' -count=1`

Expected: FAIL because the command group does not exist.

- [ ] **Step 3: Implement the Cobra command group**

Register `terminal` under root. All subcommands take `--workspace/-w`. `list` loads persisted sessions. `attach` connects to the broker and rejects unattended or non-TTY stdin/stdout. `detach` sends the same release request used by the foreground client.

- [ ] **Step 4: Implement terminal restoration**

Use project terminal helpers or `x/term` to enter raw mode, forward stdin/stdout, intercept only `0x1d` (`Ctrl-]`) for detach, and restore local terminal state with `defer` on every success/error/signal/broker-EOF path.

- [ ] **Step 5: Verify and commit**

Run: `go test ./cmd/hufu -count=1 && go build ./cmd/hufu`

Expected: PASS.

```bash
git add cmd/hufu/terminalcmd.go cmd/hufu/terminalcmd_test.go cmd/hufu/root.go cmd/hufu/options.go cmd/hufu/run.go
git commit -m "feat(cli): attach to local PTY sessions"
```

### Task 6: Add hufu TUI takeover mode

**Files:**
- Modify: `internal/tui/tui.go`
- Modify: `internal/tui/tui_test.go`
- Modify: `internal/tui/teatest_integration_test.go`
- Modify: `cmd/hufu/display.go`
- Test: `cmd/hufu/display_test.go`

**Consumes:** Tasks 3 and 5 broker client semantics.

**Produces:** Detail-view `t` takeover and `Ctrl-]` return without changing overlay priority.

- [ ] **Step 1: Write failing TUI tests**

```go
func TestDetailTStartsTerminalTakeover(t *testing.T) {}
func TestTerminalTakeoverCtrlBracketReturnsToDetail(t *testing.T) {}
func TestTerminalTakeoverResizeForwardsDimensions(t *testing.T) {}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/tui -run 'Test(DetailTStartsTerminalTakeover|TerminalTakeover)' -count=1`

Expected: FAIL because takeover mode does not exist.

- [ ] **Step 3: Add pure TUI state transitions**

Add active terminal session ID, controller/lease state, and forwarding mode. Add messages for attach output, detached, and attach error. `Model.Update` only changes state and returns commands; all broker I/O stays outside `Update`.

- [ ] **Step 4: Bridge I/O in the display layer**

In detail view `t` requests takeover. While active, forward key bytes and `tea.WindowSizeMsg` via broker commands. `Ctrl-]` releases the lease and returns to detail view. Do not change the documented `View()` overlay priority.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/tui ./cmd/hufu -run 'Test(DetailTStartsTerminalTakeover|TerminalTakeover|Display)' -count=1`

Expected: PASS.

```bash
git add internal/tui/tui.go internal/tui/tui_test.go internal/tui/teatest_integration_test.go cmd/hufu/display.go cmd/hufu/display_test.go
git commit -m "feat(tui): take over PTY task sessions"
```

### Task 7: End-to-end regression, documentation, and release gate

**Files:**
- Modify: `README.md`
- Modify: `AGENTS.md`
- Test: `internal/team/terminal_pty_test.go`
- Test: `cmd/hufu/terminalcmd_test.go`

**Consumes:** Tasks 1–6.

**Produces:** End-to-end proof and operator documentation.

- [ ] **Step 1: Write the failing end-to-end test**

```go
func TestPTYTakeoverEndToEnd(t *testing.T) {}
```

The test starts a PTY fixture, attaches a client, asserts the owner task pauses, sends input, detaches, then asserts resume handoff and finish-gate behavior.

- [ ] **Step 2: Verify failure before final integration fixes**

Run: `go test ./internal/team ./cmd/hufu -run TestPTYTakeoverEndToEnd -count=1`

Expected: FAIL until all boundary wiring is complete.

- [ ] **Step 3: Fix only integration defects exposed by this test**

Do not introduce daemon persistence, remote attach, or TREC recording. Keep scope limited to the approved in-process local broker.

- [ ] **Step 4: Document exact operator behavior**

Add this README example and explain explicit terminal permission, `Ctrl-]`, Linux/macOS support, in-process lifetime, unattended rejection, and unchanged bash semantics:

```bash
hufu --enable-pty-terminal --tui --agent-team ops "run the interactive wizard"
hufu terminal list --workspace ./workspace
hufu terminal attach <session-id> --workspace ./workspace
```

- [ ] **Step 5: Run full verification**

Run: `go test ./...`

Run: `go vet ./...`

Run: `go build ./cmd/hufu`

Expected: every command exits 0.

- [ ] **Step 6: Commit**

```bash
git add README.md AGENTS.md internal/team/terminal_pty_test.go cmd/hufu/terminalcmd_test.go
git commit -m "docs: document PTY terminal takeover"
```

