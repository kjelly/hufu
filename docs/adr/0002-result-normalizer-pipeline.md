# 0002. Team-Neutral Result Normalizer Pipeline

- Status: proposed
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

Chosen option: Phased adoption of Option A (Milestone 1) transitioning to Option B (Milestones 2–3). Option A immediately decouples domain normalization in `.agent-teams/` without runtime risk. Option B introduces a team-neutral pre-submission pipeline hook in `result-contract:` where the runtime invokes a team-declared action provider before schema validation.

### Consequences

- Good, because domain logic (AST snapping, sanitization) remains completely encapsulated in agent teams.
- Good, because dual-receipt hashing records both raw model outputs and canonical normalized forms for auditable provenance.
- Good, because normalizer failures fail-closed without consuming model retry budgets.
- Bad, because Option B adds an extra subprocess invocation during `submit_result`.

### Confirmation

The decision succeeds when `submit_result` gracefully normalizes minor line/format jitter through team action providers, zero team-specific branches exist in `internal/`, and regression tests pass across all bundled teams.
