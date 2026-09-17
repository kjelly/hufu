# Read-only runtime inspection

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-12
> Supersedes: —
> Superseded-By: —

`hufu inspect` provides one metadata-safe view across persisted run, task,
evidence, context, decision, memory, and terminal facts. It is a read-only
facade: it does not execute agents, providers, tools, verifiers, migrations,
rollback, or recovery, and it does not write workspace state.

The [unified observability inspector specification](../architecture/unified-observability-inspector.md)
defines the normative authority, identity, ordering, redaction, and replay
contracts behind this operator guide.

## Commands

```text
hufu inspect run <run-id>
hufu inspect task <task-id> --run <run-id> [--attempt <n>]
hufu inspect evidence <run-id>
hufu inspect context <task-id> --run <run-id> --project <project-id>
                     [--attempt <n>] [--team <team-id>]
                     [--agent <agent-id> | --all-agents] [--show-content]
hufu inspect trace <run-id>
hufu inspect replay <run-id>
hufu inspect storage
```

All subcommands accept workspace and format:

```text
--workspace, -w <path>   exact workspace override; defaults to the active managed workspace
--format text|json       output format; defaults to text
```

Run-scoped subcommands also accept `--branch <id|name|label>` and optional
`--session <id>`. Storage diagnostics reject those selectors because they
describe the single canonical database, not a branch or session projection.

When `--branch` is omitted, inspection is limited to the active branch. It
does not search sibling branches for a matching ID. Task and context queries
require `--run` so a task ID cannot be joined to an unrelated run or receipt.

Typical operator queries:

```bash
hufu inspect run run-123 --workspace ./workspace
hufu inspect task task-7 --run run-123 --format json
hufu inspect evidence run-123
hufu inspect context task-7 --run run-123 --project project-1 --agent worker-1
hufu inspect trace run-123 --branch incident-fix
hufu inspect replay run-123 --format json
hufu inspect storage --workspace ./workspace --format json
```

`inspect storage` reports content-free SQLite facts: page/freelist counts, page
size, journal mode, WAL autocheckpoint, schema version, database/WAL byte sizes,
and FTS/context row counts. A missing WAL is reported as zero bytes. The command
does not create storage, migrate, checkpoint, optimize, vacuum, or rebuild a
projection.

## Output and exit status

JSON output uses a versioned envelope with `schema_version`, `kind`, `query`,
`integrity`, `data`, and `diagnostics`. Consumers should select fields by name
and reject unsupported schema versions. Stable references and reason codes are
safe automation keys; rendered text and diagnostic prose are for operators.

Exit status has a command-wide meaning:

| Code | Meaning |
| --- | --- |
| `0` | Projection completed. The inspected run may still have a failed or partial outcome. |
| `1` | Canonical integrity failed, or replay detected projection drift. |
| `2` | Invalid query or format, missing/ambiguous target, or authorization denial. |

An absent optional projection is reported as `unavailable` or `skipped` and
does not by itself cause a non-zero exit. `inspect replay` compares canonical
in-memory replay with applicable stored projections. Drift reports contain
bounded field paths, not the compared raw values. A historical or non-active
run cannot use a non-run-scoped `session.json` as drift evidence, so that check
is reported unavailable instead.

## Data authority and redaction

The canonical event chain remains authoritative for run and task execution.
Evidence verdicts come from the existing audit verifier; context comes from
the read-only context repository; decision and terminal data remain
supplemental projections tied to event anchors. The inspector does not create
a second event, evidence, context, or decision store.

Output is metadata-only by default. It excludes raw prompts, transcripts,
tool arguments and output, terminal commands, environment dumps, artifact
bytes, credentials, and provider keys. `--show-content` applies only to
authorized context content and still passes through redaction. Private context
requires a matching `--agent`, or explicit maintenance access with
`--all-agents`.

## Compatibility and migration

No persisted schema or workspace migration is required. Existing commands
remain available and retain their specialized semantics:

- `hufu audit explain` and `hufu audit verify` provide detailed evidence and
  audit workflows.
- `hufu context explain` and `hufu context explain-memory` provide context
  compiler and memory-ranking details.
- `hufu decision explain` provides decision-runtime details.

Use `hufu inspect` when one safe, correlated operator view is needed; use the
specialized command when maintaining or deeply diagnosing that subsystem.
For scripts, migrate ad-hoc parsing of reports, `session.json`, or multiple
command outputs to `hufu inspect ... --format json`. There is no separate
`--json` mode for this command.
