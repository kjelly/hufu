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
hufu inspect cost [run-id] [--task <task-id>]
hufu inspect routes <run-id> [--task <task-id>]
hufu inspect economics <run-id>
hufu inspect replay <run-id>
hufu inspect semantic-retrieval [run-id] [--task <task-id>]
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

`inspect task` also reports the task's side effect. For a task dispatched from
the team's [action catalog](../reference/action-providers.md) it adds the
catalog action ID, entry and arguments hashes, the durable invocation ID, and
the linked proposal IDs; the arguments themselves are never printed. Worker
proposals appear in `inspect trace` as `team_action_proposed` events.

Typical operator queries:

```bash
hufu inspect run run-123 --workspace ./workspace
hufu inspect task task-7 --run run-123 --format json
hufu inspect evidence run-123
hufu inspect context task-7 --run run-123 --project project-1 --agent worker-1
hufu inspect trace run-123 --branch incident-fix
hufu inspect cost run-123 --task task-7 --format json
hufu inspect replay run-123 --format json
hufu inspect semantic-retrieval run-123 --task task-7
hufu inspect storage --workspace ./workspace --format json
```

`inspect semantic-retrieval` summarizes the workspace's content-free semantic
retrieval traces and the runtime status recorded when each run finished (see
[Semantic memory retrieval](../reference/semantic-retrieval.md#inspecting)).
Those files are not part of the event chain, so the envelope reports
`event_chain: not_applicable`; it rejects `--branch` and `--session`.

`inspect storage` reports content-free SQLite facts: page/freelist counts, page
size, journal mode, WAL autocheckpoint, schema version, database/WAL byte sizes,
and FTS/context row counts for `context.sqlite`. Once the workspace has a
`runtime.sqlite`, a `runtime` section reports its page facts, journal mode,
schema version, byte sizes, event rewrite count, and event, runtime-document,
session-branch, task-journal, and decision-index row counts. A missing WAL is reported as zero bytes. The command
does not create storage, migrate, checkpoint, optimize, vacuum, or rebuild a
projection.

`inspect cost` projects only canonical generation-cost events. Numeric values
use integer USD micros in JSON and fixed-six-decimal USD in text. Usage-derived,
admission-bound, and open-reservation amounts remain separate; unknown, local,
and subscription invocations are counted instead of being folded into zero.
When the run ID is omitted, the command uses the same exact active/sole-run
binding rules as `inspect overview` and fails on ambiguity.

## Observed generation economics

`hufu inspect overview --run <run-id>` also includes an optional `observations`
summary in its operator snapshot. It contains route/target distribution,
evidence-qualified task counts, the known usage subtotal, recorded-invocation
coverage and data-gap reason codes. Its `source_head` must match the snapshot's
event ID and hash; it uses the same bound lineage as the run outcome and cost
projection. The summary never changes the canonical outcome or recommends a
model.

The CLI overview and the existing TUI operator panel share a plain summary
renderer. In the TUI, open the panel with `L` and use `G` to reach the observations
at the bottom; normal scrolling and resize remain available. Missing fields in
older snapshots and failed projections display unavailable, while missing cost
amounts remain unknown. A failed latest background query clears cached operator
presentation; late older replies cannot restore it or change runtime tasks.
Scope changes also clear details belonging to the previous scope.

```sh
hufu inspect economics run-123 --workspace ./workspace --branch main --output json
```

`economics` accepts workspace, session, branch and a required run ID. Task filters
are rejected. It combines the routes projection and canonical cost ledger from
one validated event head, without a provider call or verifier execution.
`comparison_scope` is `single_run_observed`: these observations do not establish
model quality, causal savings, or a preferred route.

Amounts are integer USD micros estimated from recorded usage and prices, not
payments. `usage_cost_micros` includes failed calls and retries and is a known
subtotal. Open reservations, admission bounds, unknown prices, local execution
and subscriptions remain distinct in `cost`; unknown amounts are not zero.
`known_cost_coverage` measures usage-settled invocations divided by recorded
reservations, not the completeness of historical recording.

`cost_per_verified_task_micros` includes overhead and rounds upward. It is null
unless the run is terminal, there is at least one evidence-qualified completed
task, all invocations have usage-derived prices, and no cross-run task cost gap
exists. Historical qualification requires a validated v5 frozen cost policy
before execution, a matching admitted backend, and accounting for recorded
provider-profile invocations and worker attempts. Missing or legacy invocation
identities, unmatched profile invocations and missing attempt costs make this
qualification unavailable, even when every existing reservation is settled.
Explicit budget denial is a no-send path. This scope covers Hufu's admitted
generation boundaries; it is not an accounting of arbitrary shell, tool or
external service charges.

Tasks created or changed during the selected run form the denominator;
unchanged completed ancestors are `reused`. Missing evidence remains in the
denominator and is reported separately. Route costs exclude coordinator,
guard, judge and other shared overhead. Initial/retry/fallback amounts are
mutually exclusive and require a unique execution match. Multiple executions
within one occurrence cannot be distinguished by current cost events: their
known occurrence subtotal and unassigned subtotal remain visible, while complete
breakdowns are null with `attribution_unavailable`. This does not by itself
invalidate a fully accounted run total.

Latency uses nearest-rank p50/p95 over completed attempts with valid persisted
start/end times, with sample and exclusion counts. Empty samples are null;
running attempts never receive a live elapsed duration in this observation.

## Recorded worker routes

```sh
hufu inspect routes run-123 --workspace ./workspace --branch main --output json
```

`routes` requires a run ID and accepts an optional task filter. It binds the
exact workspace, session and branch using the existing inspector. It does not
select a model or recommend a route.

The version 1 data contains `source_head`, `tasks`, `attempts`, `attempt_count`,
`groups`, `evidence_coverage`, and content-free diagnostics. `run_outcome` and
`acceptance_state` retain their canonical meanings. Later runs do not change
a historical projection. Source branch IDs identify inherited events separately
from the queried branch.

- Task identity retains the original creation event and origin run. Resetting
  or resuming a task does not create another task occurrence. Unchanged completed
  ancestor tasks are counted as `reused`, outside the selected run's evidence denominator.
- `route_kind` is `named`, `unbound`, or `unavailable`. Named routes retain the
  durable route digest. Actual targets come from receipts; a missing target is
  empty, never silently replaced with the route's primary.
- Each dispatch retains the existing `attempt_key` and start event ID.
  Extra-model executions have separate `execution_key`, `model_execution_id`,
  and `receipt_hash`. Dispatch and occurrence attempt numbers remain separate.
- `attempt_class` is `initial`, `retry`, `fallback`, or `unavailable`.
  Fallback takes precedence over retry. Latency uses persisted timestamps;
  no clock-dependent duration is added for a running attempt.
- Legacy receipts without a unique dispatch match remain visible. When several
  already-counted dispatches could own a receipt, its row has `counted: false`
  and adds no attempt or target completion credit. `attempt_count` and group
  counts include only counted rows; attribution diagnostics mean coverage is
  incomplete, not that additional executions are known to be absent.

`completion_evidence` is `verified`, `not_verified`, or `unavailable`. A done
task needs a uniquely evidence-bound successful receipt, persisted objective
verification, and applicable offline evidence checks to be `verified`. Exit
zero, model prose, or run completion alone is insufficient. An unavailable
winner earns no target completion credit. Artifact or binding contradictions
fail with an integrity error; missing historical evidence stays unavailable.
Verifier commands and providers are never executed by the query.

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
run cannot use the non-run-scoped session checkpoint as drift evidence, so that check
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
  compiler and memory-ranking details. For historical active-query selections,
  use `hufu context explain-memory <id> --retrieval-id <retrieval> --project
  <project> --team <team>`; it reports the frozen query policy and
  `query_plan_required` without plaintext query or reranking. `hufu inspect
  context` also projects this metadata and model execution identity.
- `hufu decision explain` provides decision-runtime details.

Use `hufu inspect` when one safe, correlated operator view is needed; use the
specialized command when maintaining or deeply diagnosing that subsystem.
For scripts, migrate ad-hoc parsing of reports, `runtime.sqlite` rows, or multiple
command outputs to `hufu inspect ... --format json`. There is no separate
`--json` mode for this command.
