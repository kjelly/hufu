# Typed run inputs migration

> Status: active
> Authority: guide
> Verified-Commit: `5e3c2c5c720fede6efb1abf057b22f7977601456`
> Supersedes: —
> Superseded-By: —

Typed run inputs replace prompt/config template variables when a value changes
execution authority, scope, or provider payloads. Ordinary `--var` templating
remains supported for prose and non-authoritative configuration.

## Operator migration

The bundled review team now declares `review.scope`. Its default and prompt
resolver select the last 10 first-parent commits ending at `HEAD`:

```bash
hufu --agent-team hufu-code-review 'Review the last 10 commits'
```

For automation, pin the complete typed value:

```bash
hufu --agent-team hufu-code-review \
  --input 'review.scope={"kind":"last_n","count":3,"history":"first_parent","head":"HEAD"}' \
  'Review the selected changes'
```

`hufu-code-review-ci` is the bundled one-commit profile. Explicit input and a
scope expression in the prompt must agree; a mismatch fails before model
execution. `--var review.scope.max_commits=N` is deprecated, ignored for
execution, and emits a warning.

## Team author migration

Declare bounded schemas under `spec.inputs`, bind inputs into static actions
with `input-bindings`, and require a blocking `task_output_assert` that checks
both the requested value and its hash. An input-bound workset producer without
that assertion is rejected by team lint.

Provider outputs used by acceptance belong in `TaskResult.runtime_outputs`,
not model-authored facts. The runtime persists the input snapshot, materialized
payload hash, bound input hashes, output digest, and assertion evidence.

## Existing workspaces and branches

Run the read-only inspector before resuming historical workspaces:

```bash
hufu migrate inspect-execution --workspace ./workspace
```

The report now includes canonical run-input snapshots, input-bound tasks,
legacy unbound runtime outputs, input-binding conflicts, and active-session
projection conflicts. A workspace containing an interrupted input-bound task
must be resumed with the runtime version that created it. If its frozen
snapshot or receipt binding is missing or conflicting, start a new run with
`--new`; do not reconstruct execution authority from task prose or artifacts.

## Verification and observability

Markdown reports and `--output json` expose the terminal run-input snapshot and
input-bound assertion summaries. `hufu audit verify` independently replays the
binding and assertion dimension. Reliability metrics count resolved typed
inputs, input-bound actions, and passed/failed input-bound assertions.
