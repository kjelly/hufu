# Operator journeys

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

These journeys use only shipped commands. They keep observation separate from
mutation: inspect first, then invoke the exact action only when the displayed
state and scope support it.

## A. First verified result

Use the optional wizard only from a terminal. It previews and statically
validates files before the exact `write` confirmation. It never overwrites an
existing team, starts a provider, downloads a model, or runs the new team.

```sh
hufu team create dev-team --wizard
hufu team check dev-team
hufu run --team dev-team -- "implement the requested change and verify it"
```

For scripts, skip the wizard:

```sh
hufu team create dev-team --preset readonly --model ollama/qwen3:8b
hufu team check dev-team
```

`readonly`, `review`, or any other preset name describes Hufu tool policy; it
is not an operating-system sandbox. A preset containing `bash` may have file or
network side effects unless runtime safety flags and host controls prevent them.

## B. Continue an existing workspace

```sh
hufu inspect overview --workspace ./workspace/dev-team
hufu session status --workspace ./workspace/dev-team --team dev-team
hufu session resume --workspace ./workspace/dev-team --team dev-team --run run-123 --branch main
```

The workspace is exact. A session branch is Hufu event lineage, not a Git
branch. Resuming preserves completed work and durable execution bindings; a new
task is not the same as `--new`.

## C. Recover after interruption

Start with the three operator answers, then inspect evidence for the exact run.

```sh
hufu inspect overview --workspace ./workspace/dev-team --run run-123
hufu inspect evidence run-123 --workspace ./workspace/dev-team --branch main
hufu inspect task task-7 --workspace ./workspace/dev-team --branch main --run run-123
```

If external effects are unknown, do not retry. Use the `Next` action from the
overview. Reconcile only an active, compatible target and only after examining
the task evidence:

```sh
hufu session reconcile --workspace ./workspace/dev-team --team dev-team --run run-123 --branch main --task task-7
```

## D. Govern learning and publication

```sh
hufu context learning --workspace ./workspace/dev-team --project project-id --team dev-team
hufu context promotion list --workspace ./workspace/dev-team --project project-id --team dev-team
hufu context promotion review --workspace ./workspace/dev-team --project project-id --team dev-team
```

Recall, consultation, application, objective support, proposal, approval, and
application are distinct states. Review approval does not edit a target. Apply
requires a second exact confirmation and repeats source, target, and revision
checks. Skill authoring remains a separate `hufu skill` workflow.

## E. Long-running and e-paper terminals

```sh
hufu run --team dev-team --tui --theme light --display-preset epaper -- "monitor a long task"
```

Press `L` for the read-only operator panel. It does not run a provider,
verification, recovery, or promotion. The e-paper preset caps presentation
refreshes; it does not alter runtime state.

## F. Automation and CI

```sh
hufu run --team dev-team --quiet --output json --event-format jsonl -- "run checks"
```

Non-TTY execution never enters the wizard. Pass exact selectors, keep JSON
stdout separate from diagnostics, and treat scope ambiguity as an error rather
than attempting an interactive choice.

See the [generated command reference](../reference/operator-command-reference.md)
for the metadata-backed examples and [operator troubleshooting](operator-troubleshooting.md)
for recovery decisions.
