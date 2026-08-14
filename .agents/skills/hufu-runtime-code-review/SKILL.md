---
name: hufu-runtime-code-review
description: Review Hufu runtime-integrity changes for correctness, security, recovery safety, and projection consistency. Use for local diffs or pull requests that touch coordinator execution, task/result contracts, workflow phases, tool authorization, terminal or MCP actions, persistence, recovery, receipts, evidence, sessions, or their integration tests—especially under internal/team, internal/tools, internal/context, or cmd/hufu.
---

# Hufu Runtime Code Review

Perform a finding-first, read-only review of Hufu's cross-cutting runtime
behavior. Treat runtime state, typed results, receipts, objective verification,
and persisted evidence as authoritative; do not accept model prose as proof of
completion.

## Establish Scope

Read the repository `AGENTS.md` and the changed code in context. Identify the
intended behavior from the change description, tests, nearby contracts, and
callers. Ignore unrelated dirty-worktree changes.

Map each relevant change to its canonical surfaces before judging it:

| Change area | Inspect together |
| --- | --- |
| Task, result, or status | `todo*`, `task_result*`, `coordinator_task_run.go`, status projections |
| Events or observability | event store, reducers, execution events, CLI/JSON/report/TUI projections |
| Authorization | policy gate, deny logic, delegation policy, phase capability filters |
| Workflow or actions | runtime workflow, phase/context, contract compilation, action providers |
| Persistence or recovery | session/checkpoint, receipts, evidence/artifacts, failure disposition, anti-thrashing |
| Secrets or workspace access | redaction, audit/transcript/report/session sinks, path and artifact authorization |

## Trace Every Affected Execution Path

For each behavior change, explicitly consider normal workers, direct-agent
execution, subagents, sidecars, plan reviewers, retry/repair, runtime actions,
fast-path routing, dry-run, unattended mode, and crash-resume. Report a
concrete missed path when the code updates only a subset; do not assume a
shared-looking helper covers all callers.

## Review Invariants

### Policy and model-visible tools

- Require every agent creation and tool execution path to use the central policy
  gate. Check that the model-visible tool set and the runtime allowlist match.
- Require phase, capability, closed-sequence, unattended, path, and MCP checks
  to apply to special paths as well as ordinary workers.
- Reject designs that turn a denied tool call into an aborted model round when a
  recoverable tool error is required to preserve attempt evidence.

### Task completion and state projections

- Follow every task transition through the canonical transition API. Check the
  Todo state, checkpoint, task journal, event store, receipt, session, and
  user-facing projections together.
- Require the same completion gate on normal completion, protocol repair, and
  resumed tasks: typed-result validation, terminal evidence, objective
  verification, and acceptance where configured.
- Flag direct field mutation or a success path that bypasses a required
  projection, verifier, or terminal failure check.

### Recovery, retries, and side effects

- Confirm that failure classification, retry, repair, reconciliation, and
  anti-thrashing use the established recovery machinery.
- Require receipts and failure evidence to distinguish cancellation, timeout,
  step/token budget exhaustion, protocol incompleteness, verification failure,
  and permission denial.
- Reject automatic replay of a completed or potentially completed side effect,
  including during protocol repair or crash-resume.

### Persistence, artifacts, and secrets

- Check backward reads, migrations, and reconstruction when a persisted type or
  serialized field changes. Preserve atomic writes and failure recovery.
- Require redaction before every durable or displayed sink: subprocess output,
  transcript, audit record, receipt, session, report, event payload, and
  context store.
- Treat artifact references as opaque, content-addressed identifiers. Flag
  paths, Todo IDs, or unverified references used in place of authorized
  artifact access.

### Workflow contracts

- Require static configuration, executable availability, task references,
  phase/capability grants, and closed tool sequences to be preflighted before a
  model call or external action.
- Verify that phase restrictions filter both built-in and MCP tools, in both
  the model-visible set and execution-time authorization.
- Keep Hufu core integration-neutral; require consumer-specific behavior to
  live behind generic workflow or `ActionProvider` contracts.

## Evaluate Tests and Validation

Require focused tests for every changed invariant. Select applicable cases:

- parser/schema and static-contract rejection;
- allow/deny enforcement, including direct, repair, sidecar, and phase paths;
- normal, failed, repaired, and resumed lifecycle/projection results;
- non-replay of side effects and retry/budget behavior;
- redaction, artifact authorization, atomic persistence, and legacy-session
  compatibility.

Run proportionate read-only checks when practical. For changed Go code, name
the required repository gates (`go test ./...`, `go vet ./...`, and
`golangci-lint run`) and distinguish commands actually run from those not run.

## Report Findings

Report demonstrable findings first, ordered by severity. For each finding,
include the precise file and location, affected execution path, failing
scenario, violated invariant, and minimal fix direction. Avoid speculative
style feedback and do not alter source code, commits, or review state unless
explicitly asked.

When no finding remains, state the paths and invariants inspected, residual
untested risks, and validation performed.
