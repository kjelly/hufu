# Operator experience preview and fallback

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

The operator-experience additions are available as an additive preview. They
reuse the existing runtime, event store, verification, recovery policy, and
promotion authorization. They do not migrate workspace data or remove legacy
commands.

## Preview surfaces

- `hufu inspect overview` presents State, What, and a deterministic safe Next
  action from the canonical read-only snapshot.
- `hufu run` and the explicit session recovery facades offer exact scope while
  the legacy root invocation remains available.
- The optional TUI operator panel, compact layout, semantic themes, and
  low-refresh preset change presentation only.
- `hufu context learning` and `hufu context promotion review` expose bounded
  evidence. Approve and apply remain separate explicit operations.
- `hufu team create --wizard` is TTY-only and stops at preview, validation, and
  an exact `write` confirmation. It never runs the team.
- Generated shell completion is bounded and read-only. Nushell currently has
  static declarations but no selector-aware dynamic ID completion.

The stable label remains withheld because the required baseline/candidate
human study has no participants. See the
[release readiness report](../reference/operator-release-readiness.md).

## Opt in

Use preview commands explicitly:

```sh
hufu inspect overview --workspace ./workspace/dev-team
hufu run --team dev-team -- "implement and verify the change"
hufu run --team dev-team --tui --theme auto --display-preset epaper -- "inspect the run"
hufu context promotion review --project-id <project> --team-id <team> --workspace <path>
```

Opening a review or TUI panel does not authorize a mutation. Confirm only an
exact action whose displayed scope and freshness still match your target.

## Presentation fallback

No data rollback is required to stop using the preview UI:

```sh
# Existing root execution and workspace semantics.
hufu --agent-team dev-team -w ./workspace "implement and verify the change"

# Plain, colorless, non-animated output with no new execution summary.
hufu run --team dev-team --display-mode plain --no-color --no-spinner --no-summary -- "task"

# Keep the existing/default rendering policy and automatic theme.
hufu run --team dev-team --display-preset default --theme auto -- "task"
```

Additional fallbacks:

- Do not pass `--wizard`; use the non-interactive `team create` flags.
- Do not open the TUI operator panel; all read-only facts remain available from
  CLI inspection.
- Use `--output json` or `--quiet` for machine consumers; neither emits the
  human execution summary.
- Do not invoke promotion approve/apply if only read-only review is desired.

These controls alter presentation or command entry only. They do not disable
verification, change retry authorization, or reinterpret the workspace.

## Rollback boundary

Removing the preview presentation does not undo a promotion that an operator
already approved and applied. Restore the affected policy/skill through its
normal audited workflow and retain its history. Likewise, a separate runtime
or SQLite migration must follow its own migration rollback record; this
preview introduces no such migration.

Linux amd64 is the only platform with native engineering validation in the
current report. Linux arm64 and Darwin binaries cross-build, but native
terminal acceptance is still unverified. Windows is not a release target.
