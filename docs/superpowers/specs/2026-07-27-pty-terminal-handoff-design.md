# PTY Terminal Sessions with Human Handoff

## Status

Approved design. This document defines the first implementation of interactive
PTY sessions in hufu. It intentionally does not include an independent daemon
that survives a hufu process restart.

## Problem

The existing `bash` tool runs a single non-interactive command and returns its
output. The existing stateful `terminal` session manager keeps a child process
alive, but connects it through `StdinPipe` and ordinary output files. Neither
provides a terminal device, so interactive programs cannot reliably run their
interactive UI.

The feature must let an agent operate an interactive terminal, and let a human
safely take control of that *same* session through either the hufu TUI or a
second local terminal. While a human has control, the task and model must be
paused. After the human releases the session, the agent must resume with an
accurate representation of the current screen.

## Goals

- Add opt-in PTY support to the existing stateful terminal-session boundary.
- Preserve the current non-interactive `bash` behavior exactly.
- Support exclusive agent/user control of one session.
- Support human takeover from the hufu TUI and `hufu terminal attach <id>`.
- Pause the owner task and prevent agent model/tool progress during takeover.
- Persist lifecycle, audit, output artifacts, and enough state to diagnose a
  failed or interrupted session.
- Prevent `finish` and retry logic from treating an active or unknown PTY as
  completed work.

## Non-goals

- No change to `bash` tool semantics or its request/response API.
- No Windows support in the first release.
- No remote network attach.
- No broker daemon that outlives the owning hufu process.
- No automatic TREC recording integration in the first release. TREC may run
  inside a PTY command, but evidence-specific orchestration remains separate.

## Chosen Architecture

Extend the existing `TerminalSessionManager` with a PTY mode and give the
owning hufu process a local Unix-domain socket broker. The manager owns the
PTY master file descriptor; all clients, including a second `hufu` process,
communicate with the broker rather than trying to reopen that descriptor.

```text
Agent terminal tool ─┐
                     ├─ PTY Session Manager ── child interactive program
hufu TUI takeover ───┤          │
                     │          └─ Unix socket broker
hufu terminal attach ┘                    │
                                           └─ local attach client
```

This reuses existing terminal ownership, output artifact, process-group,
timeout, lifecycle, close-gate, and task recovery concepts. PTY is an
explicitly selected execution mode, not a change to every command execution.

## Session Model

Add these fields to `TerminalSession`:

```go
type TerminalMode string
const (
    TerminalModePipe TerminalMode = "pipe"
    TerminalModePTY  TerminalMode = "pty"
)

type TerminalController string
const (
    TerminalControllerNone  TerminalController = "none"
    TerminalControllerAgent TerminalController = "agent"
    TerminalControllerUser  TerminalController = "user"
)

Mode       TerminalMode
Controller TerminalController
LeaseID    string
Rows       uint16
Cols       uint16
AttachedAt time.Time
Screen     TerminalScreenRef
```

Old persisted sessions that omit `Mode` are interpreted as `pipe`. Existing
pipe sessions retain their current behavior.

Only one controller may own a session at a time. `agent` is the controller
after a newly started PTY session. `user` is the controller after takeover.
`none` is used after child exit, detach cleanup, or before a controller is
assigned. A lease ID identifies the active user attachment and prevents stale
clients from releasing a newer attachment.

## Tool and CLI API

The coordinator-owned terminal tool continues to expose `start`, `write`,
`read`, `close`, `list`, and `reconcile`. It gains:

```text
terminal.start   { command, working_dir, pty: true, rows, cols, timeout }
terminal.resize  { id, rows, cols }
terminal.signal  { id, signal }
```

`write` is accepted only while `Controller == agent`; it returns a clear
ownership error while a user lease is active. `resize` is allowed to the active
controller. `signal` uses a small allowlist (`INT`, `TERM`, `HUP`) and records
an audit event.

Add a `terminal` Cobra subcommand group:

```text
hufu terminal list [--workspace PATH]
hufu terminal attach <session-id> [--workspace PATH]
hufu terminal detach <session-id> [--workspace PATH]
```

`attach` is an interactive foreground client. It obtains a user lease from the
broker, sets its local stdin to raw mode, forwards stdin to the broker, and
writes broker output to stdout. `Ctrl-]` is consumed by the attach client and
means detach; every other byte is sent unchanged to the child PTY. Normal
terminal mode is restored on every exit path.

`detach` is primarily for explicit recovery/automation. The foreground attach
client sends the same detach request before it exits normally.

## Broker Protocol

The owning hufu process starts a Unix-domain socket broker at:

```text
<workspace>/logs/terminal-broker.sock
```

The parent directory is `0700`; the socket is created with `0600` permissions.
The broker is the sole holder of PTY masters and multiplexes their byte streams
to the current controller.

The protocol has a framed request/response control channel and a framed byte
stream for an attached session. Required control operations are:

```text
takeover(sessionID) -> leaseID, initial screen, task state
attach(sessionID)   -> leaseID, initial screen, task state
write(leaseID, data)
resize(leaseID, rows, cols)
detach(leaseID, optional summary)
status(sessionID)
```

On Linux the broker verifies the peer UID using `SO_PEERCRED`. On macOS the
implementation uses the platform peer credential API; it must fail closed if
the UID cannot be verified. Socket filesystem permissions are defense in depth,
not the only authorization decision.

The broker stays in-process for the first release. If hufu exits, the broker
and its PTY master descriptors disappear; a subsequent hufu process cannot
safely claim ownership of the previous child session.

## Human Handoff Flow

1. A user requests takeover from the hufu TUI or the attach CLI.
2. The broker atomically changes the session controller to `user` and issues a
   lease.
3. The coordinator marks the owner todo `paused` with reason `user takeover`.
   It cancels the active model/task context and rejects future agent terminal
   writes for that todo.
4. The PTY child continues running. The user drives it through the broker.
5. The user detaches/releases the lease.
6. The broker writes a handoff event and persists the most recent normalized
   screen snapshot. The coordinator changes the todo back to `in_progress`.
7. The next agent turn receives a compact handoff context: the reason, duration,
   optional user summary, last screen snapshot, child state, and output offset.

The task is deliberately not marked done at detach. The agent must inspect the
returned state and finish or report a blocker itself.

## hufu TUI Behavior

When a task detail view corresponds to an attachable PTY session, `t` requests
takeover. The TUI then switches into terminal-forwarding mode: keystrokes and
resize messages go to the broker rather than normal Bubble Tea key handling.
`Ctrl-]` detaches and restores the previous detail view. The TUI must restore
its own terminal mode on success, cancellation, child exit, and program error.

The TUI's status/detail views show session mode, controller, attachability, and
the time of the latest screen snapshot. This is not a new overlay competing
with existing priority order; it is a mode inside the detail view.

## PTY Runtime Details

Use a direct PTY dependency (for example `github.com/creack/pty`) instead of
relying on an incidental transitive module. Start a child process attached to a
PTY slave. The manager keeps the master FD, runs a reader goroutine that both
appends raw output artifacts and updates a bounded screen emulator, and routes
input only from the active controller.

Window-size changes use the PTY resize API and deliver `SIGWINCH` through the
controlling terminal. The screen emulator normalizes ANSI control sequences for
agent context; raw bytes remain available only in the terminal output artifact.

The first version must impose bounded output and screen memory. `terminal.read`
returns both the incremental raw-safe text representation and a normalized
screen snapshot. Model context must use the normalized screen, truncated to a
documented byte/rune limit.

## Failure, Recovery, and Finish Semantics

| Condition | Required behavior |
|---|---|
| Attach client disconnects | Broker detects EOF, revokes lease, leaves todo paused. |
| User releases normally | Persist handoff, resume todo for the agent. |
| PTY child exits | Persist exit code and final screen; agent handles outcome next turn. |
| hufu process exits/restarts | Persist session as `unknown`; do not retry or claim completion. |
| Running user lease | `finish` rejects completion. |
| Running/unknown PTY | Existing terminal close gate rejects finish and unsafe retry. |
| Broker startup/security error | Do not start an attachable PTY; return a clear tool error. |

`--unattended` disables user takeover and external attach. A no-human run must
not become permanently paused waiting for a person.

## Security and Privacy

- Keep existing path policy, no-network namespace, tool permission, task
  ownership, process-group cleanup, and audit hooks in the PTY path.
- Enable terminal access only for agents explicitly granted the `terminal` tool;
  do not add it to the default Helper tool set.
- Record takeover, attach, resize, signal, detach, child exit, and broker
  authorization failures as audit/events.
- Do not display raw terminal output in the general status line or external
  notifications. Interactive output may contain credentials or secrets.
- Keep screen snapshots and raw artifacts workspace-local and apply existing
  redaction/sensitive-output rules before model context or reports.

## Feature Flag and Compatibility

Gate PTY creation behind `--enable-pty-terminal` in the first release. A team
or agent still needs the explicit `terminal` tool permission. Requesting
`pty:true` while the flag is disabled returns an actionable error. Linux and
macOS are supported; unsupported platforms fail explicitly rather than silently
falling back to pipe behavior.

## Test Plan

### Unit tests

- Controller/lease state transitions and stale lease rejection.
- Agent/user write mutual exclusion.
- User detach, abrupt socket EOF, and broker lease cleanup.
- Session serialization compatibility for old pipe records.
- Signal allowlist, resize validation, and peer credential authorization.
- ANSI parsing and bounded normalized screen snapshots.

### PTY integration tests

- Start a real interactive fixture under a PTY and verify input/output.
- Verify resize produces the expected child-observed terminal dimensions.
- Verify EOF, `SIGINT`, child exit code, timeout, and process-group cleanup.
- Verify raw artifact persistence and screen snapshot output.

### Multi-process end-to-end tests

- Start hufu with a PTY session, attach from a second hufu process, type input,
  detach, and verify the owner todo pauses then resumes.
- Verify a second concurrent attach is rejected.
- Verify attach-client crash leaves the todo paused and never grants agent write
  control implicitly.

### TUI tests

- Verify detail-view takeover key handling and `Ctrl-]` restoration.
- Verify overlay priority is unchanged and terminal mode does not leak after an
  error.
- Verify `tea.WindowSizeMsg` sends PTY resize while takeover is active.

## Acceptance Criteria

- Existing non-interactive bash tests and behavior remain unchanged.
- An explicitly enabled agent can start an interactive PTY child.
- Both the hufu TUI and `hufu terminal attach <id>` can take exclusive control.
- During takeover, no agent terminal input or model progress occurs for the
  owner task.
- After release, the agent receives a trustworthy, bounded screen handoff.
- Attach failures, client crashes, and hufu restarts cannot falsely mark work
  as complete or permit unsafe automatic retry.
- `finish` rejects active or unresolved PTY sessions.
