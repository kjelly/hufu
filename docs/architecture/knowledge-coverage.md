# Knowledge-state attribution and task coverage

> Status: active
> Authority: normative

Hufu attributes deterministic knowledge state to context that actually enters
a model prompt and persists the attribution in the context injection manifest.
The runtime derives the state from existing authority and memory outcome
evidence; agents and users cannot submit or override it.

## Item states

- `known`: normative repository context, or historical context with enough
  verified support from independent tasks.
- `assumed`: eligible historical context without enough independent support.
- `stale`: supported historical context whose last observation is older than
  `memory-learning.stale-after`.
- `conflicting`: reserved for a future deterministic contradiction model; the
  current classifier never emits it.

Examples are left unclassified. Repository invariants keep their dedicated
severity and attestation fields instead of receiving a second knowledge-state
label. Omitted context and legacy manifests also have no inferred state.

## Per-task coverage

`TaskResult.KnowledgeCoverage` contains two traceable count sets:

- outcome coverage counts included manifest items classified as known,
  assumed, or stale;
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
