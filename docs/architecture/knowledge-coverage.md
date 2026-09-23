# Knowledge-state attribution and task coverage

> Status: active
> Authority: normative
> Verified-Commit: `68451de`
> Supersedes: —
> Superseded-By: —

Hufu attributes deterministic knowledge state to context that actually enters
a model prompt and persists the attribution in the context injection manifest.
The runtime derives the state from existing authority, memory outcome
evidence, and persisted memory conflict judgments; agents and users cannot
submit or override it.

## Item states

- `known`: normative repository context, or historical context with enough
  verified support from independent tasks.
- `assumed`: eligible historical context without enough independent support.
- `stale`: supported historical context whose last observation is older than
  `memory-learning.stale-after`.
- `conflicting`: shared persistent historical context with an open memory
  conflict, meaning a persisted pair judgment from `hufu context conflicts
  scan` whose derived state is open (see the
  [context SQLite schema](../reference/context-sqlite-schema.md)). It takes
  precedence over the other historical states. Given the persisted judgments
  the classification is deterministic; the judgments themselves are produced
  offline by an operator-run model call, never during a run. The marking only
  changes attribution: the compiled prompt, included and omitted items,
  ordering, and token counts are identical with or without conflicts. It is
  still marked when the conflicting counterpart was not injected. A failed
  conflict lookup leaves items unmarked and reports degraded observability.

Examples are left unclassified. Repository invariants keep their dedicated
severity and attestation fields instead of receiving a second knowledge-state
label. Omitted context and legacy manifests also have no inferred state.

## Per-task coverage

`TaskResult.KnowledgeCoverage` contains two traceable count sets:

- outcome coverage counts included manifest items classified as known,
  assumed, stale, or conflicting (`conflicting_count`, omitted when zero);
- invariant coverage counts touched paths, applicable catalog invariants, and
  paths with no applicable invariant.

An empty touched-path set produces zero invariant-coverage counts, matching the
invariant router's unscoped fail-safe behavior. The runtime computes coverage
from the persisted context manifest and invariant catalog without calling a
model or querying memory again. Reports and `hufu inspect task` expose the
counts without context content.

Knowledge coverage is diagnostic only. It is not read by completion,
acceptance, semantic-regression, retry, escalation, or autonomy policy. Missing
or invalid attribution therefore leaves coverage absent and never fails a task.

The implementation is [`internal/team/knowledge_state.go`](../../internal/team/knowledge_state.go),
with manifest attribution in [`context_manifest.go`](../../internal/team/context_manifest.go).
