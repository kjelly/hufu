# Terminal cleanup and custody transfer plan

> Status: proposal
>
> Scope: stateful `terminal` sessions, especially PTY-backed interactive
> children whose owner task fails, is cancelled, or reaches a terminal state
> before the child does.
>
> Related code: `internal/team/terminal_session.go`,
> `internal/team/coordinator_task_run.go`,
> `internal/team/coordinator_run.go`, and
> `internal/team/coordinator_tools_terminal.go`.

## 1. Problem statement

Hufu already treats a terminal session as an owned task resource:

- `TerminalSession.OwnerTaskID` records the creating task.
- `ownerSessionLocked` rejects read/write/close calls whose context task ID is
  not that owner.
- `RequireTaskClosed` prevents an owner task from being marked successful
  while one of its sessions is `running` or `unknown`.
- `RequireNoLeaks` prevents `finish` from accepting a run with an active or
  unknown session.

Those are correct safety boundaries, but there is a lifecycle gap.  When a
worker starts an interactive child and then finishes or fails without closing
it, `RequireTaskClosed` turns the task into an error.  A follow-up task cannot
repair the leak because it has a different task ID and `Close` rejects it.
The resource remains alive, the run cannot finish, and a later forced kill
loses the audit trail that explains why it was safe.

This is not a reason to weaken task ownership.  A random follow-up agent must
not be allowed to type into, close, or reassign another task's terminal.  The
missing capability is a coordinator-owned cleanup path with an explicit,
auditable custody transfer.

## 2. Goals and non-goals

### Goals

1. A task reaching `done`, `error`, `blocked`, `cancelled`, or an abandoned
   retry boundary cannot leave a live child indefinitely.
2. Cleanup is deterministic and does not depend on an LLM deciding to invoke
   `terminal close` correctly.
3. The original task remains the immutable provenance owner; a later task
   never silently becomes its owner.
4. The coordinator can acquire narrowly-scoped cleanup custody after the
   owner task is no longer executing, terminate the verified process group,
   and record the outcome.
5. After a hufu restart, an `unknown` session remains fail-closed.  Cleanup
   may act only after process identity verification; PID reuse must never be
   killed speculatively.
6. The result, event stream, status projection, and CLI make the distinction
   between a clean terminal exit, automatic cleanup, and manual intervention
   visible.

### Non-goals

- Do not let an arbitrary task, agent, or broker client close another task's
  terminal.
- Do not transfer a running PTY directly to a retry or replacement task in
  this phase.
- Do not make `terminal` a service supervisor.  A service that is intended to
  outlive its task must use an explicit service/deployment mechanism, not a
  leaked terminal session.
- Do not infer a successful task result from a terminal exit or its output.

## 3. Design principles

1. **Provenance is immutable.** `OwnerTaskID` always names the task that
   created the child.  It is never overwritten during recovery.
2. **Custody is temporary and capability-scoped.** Only the coordinator's
   lifecycle code can hold cleanup custody.  Model-visible terminal tools keep
   the current owner-task checks.
3. **A terminal state is not a task outcome.** Process exit, cleanup success,
   and task verification remain separate facts.
4. **Termination is staged and bounded.** Record graceful termination,
   escalation, and final process observation; use the existing process-group
   boundary so descendants do not leak.
5. **Unknown stays fail-closed.** A restored session cannot be closed merely
   because a PID happens to exist.  It needs matching `ProcessIdentity`.
6. **One cleanup operation wins.** Start, owner close, child exit, task
   finalization, run cancellation, and restart reconciliation must be
   idempotent and race-safe.

## 4. Target lifecycle model

Keep the existing process state (`running`, `exited`, `closed`, `unknown`) and
add an orthogonal cleanup/custody record:

```go
type TerminalCustodian string

const (
    TerminalCustodianOwner       TerminalCustodian = "owner_task"
    TerminalCustodianCoordinator TerminalCustodian = "coordinator_cleanup"
    TerminalCustodianOperator    TerminalCustodian = "operator"
)

type TerminalCleanupState string

const (
    TerminalCleanupNone       TerminalCleanupState = "none"
    TerminalCleanupRequested  TerminalCleanupState = "requested"
    TerminalCleanupGraceful   TerminalCleanupState = "graceful_termination"
    TerminalCleanupForced     TerminalCleanupState = "forced_termination"
    TerminalCleanupCompleted  TerminalCleanupState = "completed"
    TerminalCleanupManual     TerminalCleanupState = "manual_intervention"
)

type TerminalCleanupReason string

const (
    TerminalCleanupTaskFailed     TerminalCleanupReason = "task_failed"
    TerminalCleanupTaskCancelled  TerminalCleanupReason = "task_cancelled"
    TerminalCleanupTaskIncomplete TerminalCleanupReason = "task_incomplete"
    TerminalCleanupRunCancelled   TerminalCleanupReason = "run_cancelled"
    TerminalCleanupRunShutdown    TerminalCleanupReason = "run_shutdown"
)
```

Add the following fields to `TerminalSession`:

```go
Custodian          TerminalCustodian    `json:"custodian,omitempty"`
CleanupState       TerminalCleanupState `json:"cleanup_state,omitempty"`
CleanupReason      TerminalCleanupReason `json:"cleanup_reason,omitempty"`
CleanupRequestedAt time.Time            `json:"cleanup_requested_at,omitempty"`
CleanupCompletedAt time.Time            `json:"cleanup_completed_at,omitempty"`
CleanupError       string               `json:"cleanup_error,omitempty"`
```

Backward compatibility: a record without these fields reads as
`custodian=owner_task` and `cleanup_state=none`.

### Custody transitions

| Trigger | Preconditions | Custodian/action | Result |
|---|---|---|---|
| Owner calls `terminal.close` | Owner task context matches and child is live | `owner_task`; existing close path | `closed`/`exited` with owner-close event |
| Child exits naturally | Any controller | no transfer | `exited`, resource released |
| Owner task returns success | `RequireTaskClosed` passes | no transfer | task may become `done` |
| Owner task returns non-success or fails close gate | Owner model round has stopped | coordinator obtains cleanup custody | staged cleanup, then task remains error/blocked for its original cause |
| Run cancellation/shutdown | no live owner model round | coordinator obtains cleanup custody | staged cleanup before final run result |
| Restarted `unknown` session | verified identity is dead | coordinator reconciles | `exited`, resource released |
| Restarted `unknown` session | identity mismatch, unsupported verification, or child survives termination | no unsafe claim | `manual_intervention`; run is blocked |

The transition to `coordinator_cleanup` is the required ownership transfer for
this problem.  It transfers *cleanup authority*, not task provenance or agent
input control.  It must be impossible through the worker-facing `terminal`
tool API.

## 5. New manager API

Add a coordinator-only interface rather than overloading `Close`:

```go
type TerminalCleanupRequest struct {
    OwnerTaskID string
    Reason      TerminalCleanupReason
    GracePeriod time.Duration
    ForceAfter  time.Duration
}

type TerminalCleanupResult struct {
    Session      TerminalSession
    Graceful     bool
    Forced       bool
    ManualAction bool
}

type TerminalCleanupManager interface {
    CleanupTaskTerminals(context.Context, TerminalCleanupRequest) ([]TerminalCleanupResult, error)
    CleanupRunTerminals(context.Context, string, TerminalCleanupReason) ([]TerminalCleanupResult, error)
}
```

`TerminalSessionManager` implements this interface, but the method is not
exposed as a model-callable action.  It validates, under the manager mutex:

1. the session belongs to `OwnerTaskID` and the task no longer has an active
   model round;
2. no other cleanup operation owns the session;
3. a user lease is revoked/marked closing before child termination;
4. an in-memory child has an owned `exec.Cmd`, or a restored child has a
   matching persisted process identity.

For a live in-process child, cleanup should:

1. persist `cleanup_requested` and coordinator custody;
2. close stdin/PTY input and invalidate any user lease;
3. signal the process group with `SIGTERM` (PTY may additionally receive
   `SIGHUP` when its controlling session closes);
4. wait for `GracePeriod` using the existing session `done` channel;
5. if still alive, signal the process group with `SIGKILL`, wait for a bounded
   `ForceAfter`, and persist the final observation;
6. preserve the raw output artifact and emit resource-release only after the
   output copier/file is closed.

If graceful or forced cleanup cannot establish a dead process, persist
`cleanup_state=manual_intervention` with the exact reason.  Do not set
`closed`, do not release the run gate, and do not retry the original task as if
the terminal never existed.

The current `Close` implementation jumps directly to `SIGKILL`.  It remains a
valid explicit owner operation for compatibility, but the coordinator cleanup
path should be staged so its audit record explains what happened.

## 6. Coordinator integration

### 6.1 Finalize every task through one path

Create one helper near the task execution boundary, for example:

```go
func (c *Coordinator) finalizeTaskTerminalResources(
    ctx context.Context,
    todoID string,
    taskErr error,
) error
```

Call it before each terminal status transition in both normal worker execution
and direct-agent execution:

1. Let a naturally exited session pass unchanged.
2. If the worker reported success but `RequireTaskClosed(todoID)` fails, make
   the task error `unclosed terminal session`, then invoke cleanup with reason
   `task_incomplete`.
3. If the worker/model/tool execution already failed, invoke cleanup with
   `task_failed` before recording the final failure event and receipt.
4. If cleanup succeeds, retain the original execution failure; cleanup is
   evidence that the resource was contained, not proof that the task worked.
5. If cleanup fails or needs manual action, promote the final task state to
   `blocked` with failure class `terminal_cleanup`, include the session ID and
   evidence refs, and prevent automatic replay unless the recovery policy
   explicitly reconciles it.

The helper must run after the agent's model context is cancelled/unregistered,
so it cannot race an owner `terminal.write`.  Existing `registerTerminalRound`
and `unregisterTerminalRound` provide the necessary lifecycle hook; add a
deterministic `isTerminalRoundActive(todoID)` check under the same lock.

### 6.2 Run shutdown and wrap-up

Run cancellation, second Ctrl-C, timeout/budget stop, and coordinator teardown
must call `CleanupRunTerminals` before final session persistence and broker
shutdown.  The cleanup context must be independent of the cancelled model
context and have a small, explicit timeout.

Ordering:

```text
stop new work → cancel owner model rounds → cleanup live terminal resources
→ persist task/run result → close broker/event store
```

The broker must stop accepting new attachments once cleanup begins.  An active
attach client receives a terminal-close response rather than silently retaining
the lease.

### 6.3 Retry and continuation

Current retries retain the same todo ID, so they need no ownership transfer.
The retry starts only after cleanup reports `completed` or reconciliation
proves the child exited.  A replacement task with a different ID must not
adopt a still-running session in this release.

If future workflows need handoff to a different task, add a separate,
operator-authorized `transfer` operation with all of the following:

- both source and destination are in the same run;
- source task is paused, not terminal, and has no active model round;
- the session has no user lease;
- destination explicitly declares it accepts the session ID and terminal mode;
- an event records source, destination, reason, and operator authorization.

Do not bundle this feature into automatic cleanup; it has fundamentally
different safety semantics.

## 7. Observability and user-facing behavior

Add lifecycle events with structured session and task identity:

```text
terminal_cleanup_requested
terminal_cleanup_graceful
terminal_cleanup_forced
terminal_cleanup_completed
terminal_cleanup_manual_intervention
terminal_custody_transferred
terminal_user_lease_revoked_for_cleanup
```

Status projection should show:

- `working` while a session is owner-controlled and active;
- `paused` while a human lease is active;
- `error` for an unknown or manual-intervention terminal;
- a concise cleanup result in the owning task detail, never raw terminal
  output.

Extend `hufu terminal list` and JSON status with cleanup state, custody,
reason, timestamps, and the output artifact reference.  The CLI should state
whether a session is safe to retry, already contained, or requires a human.

## 8. Implementation sequence

### Phase A — manager state and safe cleanup primitive

Files:

- `internal/team/terminal_session.go`
- `internal/team/terminal_session_test.go`
- `internal/team/terminal_waiter.go` (only if a cleanup wait target improves
  reuse)

Deliverables:

1. Persist cleanup/custody fields with legacy defaults.
2. Add manager-only task/run cleanup APIs.
3. Implement graceful-then-force process-group termination.
4. Preserve PID-identity fail-closed behavior for restored sessions.
5. Emit lifecycle events in a deterministic order.

### Phase B — coordinator task and run finalization

Files:

- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_run.go`
- `internal/team/coordinator_terminal.go`
- `internal/team/coordinator_terminal_test.go`

Deliverables:

1. One task-finalization cleanup path for worker and direct-agent execution.
2. Cleanup before retry/error/done persistence, with the original error
   retained when containment succeeds.
3. Run cancellation/wrap-up cleanup independent of model cancellation.
4. Broker closure ordering and user-lease revocation.

### Phase C — projection, reporting, and operator diagnostics

Files:

- `internal/team/status_projection.go`
- `internal/team/status_projection_test.go`
- `cmd/hufu/terminalcmd.go`
- `cmd/hufu/terminalcmd_test.go`
- relevant result/report renderers

Deliverables:

1. Render cleanup state without exposing raw output.
2. Show actionable retry/manual-intervention guidance.
3. Ensure a completed cleanup no longer causes a false leaked-session gate.

### Phase D — explicit cross-task handoff

The coordinator now exposes a deliberately non-model-callable
`TransferTerminal` operation for an operator-approved repair handoff. It
requires the source and destination tasks to belong to the active run, the
source to be paused with no active model round, the destination to be
non-terminal with no active model round, and the destination's explicit
acceptance of both session ID and terminal mode. A live user lease, cleanup,
or custody transition prevents the transfer.

The durable record keeps `OwnerTaskID` as immutable provenance and records the
current `ControllerTaskID`, handoff reason, operator authorization, and time.
Normal terminal actions authorize the controller task, so no task receives
access absent this explicit transfer. `terminal_task_transferred` records the
source, destination, reason, and authorization. Cleanup, completion gates,
broker reads, and status projection follow the controller task after transfer.

For a running local PTY broker, an operator invokes the same coordinator-owned
path with `hufu terminal transfer <session-id> <destination-task-id> --mode pty
--reason <reason> --authorization <incident-or-approval>`. The broker requires
the caller to be detached and never exposes transfer through the model-facing
`terminal` tool.

## 9. Test plan

### Unit tests

1. A terminal record without cleanup fields loads with legacy defaults.
2. A non-owner task still cannot call owner-facing `write`, `read`, or
   `close`.
3. Coordinator cleanup can acquire custody only for the owner task after its
   model round is inactive.
4. Concurrent owner close, child natural exit, and coordinator cleanup yield
   one final state and one resource release.
5. Cleanup sends graceful termination, escalates after the grace timeout, and
   records both events.
6. A restored session with a mismatched process identity becomes
   `manual_intervention`; no signal is sent to that PID.
7. A live user lease is revoked before cleanup and cannot write afterwards.
8. Repeated cleanup calls are idempotent.

### Coordinator integration tests

1. A worker starts a PTY then reports success without closing it: the task is
   not `done`, the process group is cleaned up, and its output artifact is
   retained.
2. A worker starts a PTY then fails: cleanup occurs before the failure is
   finalized; the original failure remains visible.
3. Cleanup failure yields `TaskBlocked` and disables blind retry.
4. A retry of the same todo ID starts only after the prior terminal is
   contained.
5. Run cancellation cleans every active session before broker/event-store
   teardown.
6. Restarted `unknown` sessions require reconciliation and cannot be hidden by
   a new task.

### PTY and process-group tests

1. Use a parent/child fixture so graceful and forced cleanup prove descendants
   do not survive.
2. Verify both pipe and PTY modes preserve artifacts and exit facts.
3. Verify a broker-attached user sees a clear closure response during run
   shutdown and the local terminal mode is restored.
4. Run the process-group tests under the existing `NetworkBlock` coverage to
   preserve the `SysProcAttr` merge regression protection.

### Required verification commands for implementation

```bash
go test ./internal/team -run 'TestTerminal|TestCoordinator.*Terminal|TestStatusProjection' -race -count=1
go test ./cmd/hufu -run 'TestTerminal' -count=1
go test ./... -count=1
go vet ./...
golangci-lint run
git diff --check
```

## 10. Acceptance criteria

The change is complete only when all of the following are true:

1. A task cannot end with a live PTY or pipe child merely because the agent
   omitted `terminal.close`.
2. A follow-up task cannot operate another task's terminal through the normal
   tool API.
3. The coordinator can contain the leaked child using an auditable custody
   transition and verified process-group cleanup.
4. Cleanup never marks the original task successful and never hides its
   original failure.
5. Unknown/restarted sessions stay fail-closed unless process identity proves
   the correct child is gone or can safely be terminated.
6. Final acceptance no longer deadlocks on a session that was successfully
   contained, while it still rejects manual-intervention sessions.
7. The full test matrix above passes, including race and process-descendant
   coverage.
