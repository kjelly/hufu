# Hufu run outcome

> Status: active
> Authority: normative
> Verified-Commit: `6ab9951`
> Supersedes: `archive/implementation-plans/incomplete-run-reliability.md`
> Superseded-By: —

`EvaluateRunOutcome` is the sole policy for deriving the run outcome and goal
satisfaction from observed runtime state. Presentation layers must not infer
success from coordinator prose, a worker result, or an empty task list.

## Canonical outcomes

```text
completed   unverified   partial   blocked   failed   cancelled   stalled
```

- `completed` means there are no unresolved tasks and acceptance passed.
- `unverified` means the run stopped without an acceptance contract; output may
  be useful, but automation must not treat it as proof of the requested goal.
- `partial` covers unresolved work, budget/interruption stops, and failed
  acceptance after task execution.
- `blocked` means a task is blocked by policy, contract, or an external
  dependency that cannot be safely resolved by the runtime.
- `failed` is a run-level execution failure.
- `cancelled` and `stalled` retain their distinct operator-visible causes.

`AcceptanceNotConfigured`, `AcceptancePassed`, and `AcceptanceFailed` are
distinct states. Missing acceptance is never silently upgraded to passed.

## Evaluation precedence

The evaluator applies these terminal signals in order:

```text
stalled
  → cancelled
  → run failure
  → budget exceeded / coordinator interruption
  → blocked or unresolved task
  → failed acceptance
  → acceptance not configured (unverified)
  → acceptance passed (completed)
```

This order preserves the most specific safety signal. Unknown persisted
acceptance states fail closed as `AcceptanceFailed`.

`CompletionGate` is the separate final evidence gate. It can downgrade a
claimed success when required tasks, verification, evidence, acceptance, risk,
or terminal-resource invariants are not satisfied; it cannot turn an
unverified or failed run into a success merely because a model asked to
finish.

## Evidence boundaries

- Task `done` requires the task's declared verifier to pass.
- Dependencies unlock only from canonical task state, not from rendered logs
  or reports.
- Run acceptance is an objective whole-run gate and is independent of the
  coordinator's final prose.
- Reports, TUI state, JSON output, notifications, and status text are
  projections of `RunResult`.

The implementation is [`internal/team/run_result.go`](../../internal/team/run_result.go)
(`EvaluateRunOutcome`) and [`completion_gate.go`](../../internal/team/completion_gate.go)
(`CompletionGate`).
