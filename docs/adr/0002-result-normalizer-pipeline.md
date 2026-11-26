# 0002. Team-Neutral Result Normalizer Pipeline

> Status: draft
> Authority: reference
> Verified-Commit: `56f9ac73`
> Supersedes: —
> Superseded-By: —

- Date: 2026-11-24
- Deciders: Hufu Core & Agent-Team Maintainers

## Context and Problem Statement

LLM workers frequently produce minor formatting deviations, line-number offsets (e.g., citing a doc comment or function header adjacent to an assigned slice), or unnormalized URLs. When workers invoke `submit_result`, rigid Draft 2020-12 schema validation immediately rejects the submission, triggering expensive result-only repair loops or tempting maintainers to leak team-specific tolerance rules into Core Runtime. How can Hufu support deterministic pre-validation normalization while preserving strict Core / Agent-Team boundary isolation?

## Decision Drivers

- Core Runtime (`cmd/`, `internal/`) must remain 100% team-neutral without domain assumptions.
- Maintainers must not leak AST parsing, line-snapping, or URL rules into core code.
- Verifiable provenance: both raw model submissions and normalized outputs must be durably recorded.
- Fault attribution: normalizer crashes or timeouts must be classified as action failures, not model hallucinations.

## Considered Options

1. Ad-hoc in-flight hacks in Core Runtime (rejected: breaches boundaries, causes regressions).
2. Two-stage DAG workflow (Option A: Draft Worker Task -> Normalizer Go Action Task).
3. Core Runtime Team-Neutral Result Normalizer Pipeline (Option B: `result-contract.normalizer`).

## Decision Outcome

Chosen option: Phased adoption of Option A (Milestone 1) transitioning to Option B (Milestones 2–3). Option A immediately decouples domain normalization in `.agent-teams/` without runtime risk. Option B introduces a team-neutral pre-submission pipeline hook in `result-contract:` where the runtime invokes a team-declared action provider before final schema validation. The first implementation admits only registered trusted-static Go providers on Linux and rejects unsupported submission backends during preflight.

### Consequences

- Good, because domain logic (AST snapping, sanitization) remains completely encapsulated in agent teams.
- Good, because dual-receipt hashing records both raw model outputs and canonical normalized forms for auditable provenance.
- Good, because normalizer failures fail-closed without consuming model retry budgets.
- Bad, because Option B adds an extra subprocess invocation during `submit_result`.

The child receives the bounded JSON draft on stdin and returns one JSON object
on stdout. Its environment and Go imports are restricted to data processing.
The parent applies a 30-second deadline, shortened by the provider timeout
when applicable. Linux enforces an address-space limit in the child.

The submission occurrence owns the staged output. The runtime durably reserves
the invocation before starting the child. Prepared commit, read-only recovery
assessment and replay consume recorded evidence. A reservation with no durable
output stays blocked. The receipt binds both payload hashes and the hash of
the action's emitted bytes. Create-only records under the runtime control root
independently bind the original dispatch, the action output and completed
verification outcomes. The original bytes and their independent digest are
published together as one sealed record, so a failed SQLite projection can
be completed on restart without running the action or verification again.
Command verification masks credentials before sealing its diagnostics.
Successful isolated verification also binds the full workspace delta manifest.
The runtime compares that digest before preparing or replaying an apply.
The verified manifest and its apply intent are sealed in a single record
before publication. Recovery checks the sealed records before reusing a
checkpoint or starting a sibling. Historical dual-hash results remain readable
when the stored receipt matches the original admitted schema and both payload
digests.

The runtime rejects any draft that secret redaction would change before
starting the child. If redaction would change the emitted JSON, the runtime
records an action failure. Successful replay uses the exact previously sealed
bytes and verifies their digests, including the original draft's digest.

### Confirmation

The decision succeeds when `submit_result` gracefully normalizes minor line/format jitter through team action providers, zero team-specific branches exist in `internal/`, and regression tests pass across all bundled teams.
