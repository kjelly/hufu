# Operator troubleshooting and recovery

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

Begin every incident with one read-only command:

```sh
hufu inspect overview --workspace ./workspace/dev-team
```

Its normal text output answers three questions without requiring inspection of
SQLite or JSONL files:

1. **State — what is happening?** Read `State`, `Activity`, `Outcome`,
   `Acceptance`, and `Completion`.
2. **What — why is it in that state?** Read `What`, `Attention`, `Integrity`,
   and the latest reason-coded changes.
3. **Next — what is the safest next action?** Read `Next`. It is a
   deterministic suggestion, not mutation authorization.

## Scope or selector failure

Confirm `Workspace`, `Team`, `Run`, and `Branch` in the overview. Use
`hufu session status` with the same exact selectors. Do not guess a sibling
branch or combine `--workspace` with `--workspace-root`.

## Result collection recovery

```sh
hufu status --team dev-team
hufu status --team dev-team --json
```

Plain `hufu status` also shows recovery hints for the canonically selected
run, without requiring `--workers`. `--json` includes the same information
under `recovery`: unresolved task IDs, scoped inspection actions, and, for a
blocked result collection, an `assess-result-repair` action containing
`resume --repair-result <task-id> --dry-run`. Its availability describes the
read-only assessment command, not permission or a guarantee that repair will
succeed. Run the preview from the original project directory with the original
configuration and model overrides; it may still report policy drift, stale
sources, or exhausted recovery authority. Status itself does not call a model,
assess live repair eligibility, grant turns, or resume execution.

## Interrupted work with known-safe effects

Inspect the task and evidence first. If the overview recommends resume, use
`hufu session resume` with its exact run and branch. If it recommends retry,
provide the exact task; the command rechecks active-target compatibility and
recovery eligibility before dispatch.

## Unknown external effect

Do not retry. Inspect the task/evidence, then use `hufu session reconcile`
only if the overview exposes it as eligible. Reconciliation records the
operator decision at the canonical owner; it is not inferred from logs.

## Integrity degraded or invalid

Keep the workspace unchanged and use read-only detail surfaces such as
`hufu inspect replay`, `hufu inspect evidence`, and `hufu audit verify`.
Do not use completion, refresh, or a UI action as a repair mechanism. Those
surfaces intentionally return unknown/empty when their store cannot be read.

## Learning data unavailable

```sh
hufu context learning --workspace ./workspace/dev-team --project project-id --team dev-team
```

`unknown` is not zero. A missing store, unsupported schema, or failed query is
reported as unavailable; it does not prove that no recall, outcome, or proposal
exists. Promotion review is human-only and approval remains separate from
application.

## Provider or model readiness

Use `hufu team check TEAM` for static compiler/policy checks. Opt in to bounded
network checks with `hufu team check TEAM --online`. A skipped online check
does not mean the provider is available or unavailable.

If normal interfaces cannot uniquely establish scope, stop before mutation and
preserve the workspace for inspection.
