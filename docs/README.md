# Hufu documentation map

> Status: active
> Authority: normative
> Verified-Commit: `6ab9951`
> Supersedes: the undocumented flat `docs/` layout
> Superseded-By: —

This directory is organized by document lifecycle and authority. The file you
are reading is the navigation and precedence contract; it is not a substitute
for code, tests, or an accepted ADR.

## Authority order

When two sources disagree, use this order:

```text
code + tests
    > accepted ADR
    > active normative architecture/spec
    > reference
    > guide
    > deprecation record
    > archive / postmortem / historical plan
```

An implementation plan or report in `archive/` must not be used as a request
to re-create an abstraction that already exists in the runtime.

## Active documentation

### Architecture and normative contracts

- [Execution runtime](architecture/execution-runtime.md) — canonical
  `ExecutionTarget` / `ExecutionBackend` model, admission, replay, and legacy
  compatibility.
- [Decision-aware runtime](architecture/decision-runtime.md) — decision
  stages, evidence, deterministic aggregation, routing, and commit gates.
- [Run outcome](architecture/run-outcome.md) — the policy implemented by
  `EvaluateRunOutcome` and `CompletionGate`.
- [Model metadata](architecture/model-metadata.md) — model profile evidence,
  runtime introspection, and effective context admission.
- [Workset](architecture/workset.md) — artifact-backed fan-out, immutable
  expansion receipts, typed results, and group verification.
- [Memory learning](architecture/memory-learning.md) — outcome attribution,
  replayable experience, and L3/L4 rollout boundaries.
- [Memory promotion](architecture/memory-promotion.md) — review-gated LTM,
  policy, and skill promotion.
- [Strict verification design](architecture/strict-verification.md) — a draft
  mechanism design and acceptance-case reference; code and tests remain
  authoritative for implementation status.

### Guides and references

- [Decision authoring](guides/decision-authoring.md)
- [SSH tool](guides/ssh-tool.md)
- [Canonical context SQLite schema](reference/context-sqlite-schema.md)
- [Improvement artifact schemas](reference/improvement-artifact-schemas.md)
- [Agent definition format](reference/agent-format.md)
- [Skill discovery](reference/skill-discovery.md)
- [Execution compatibility sunset](deprecations/execution-compatibility.md)
- [Workset path fan-out deprecation](deprecations/workset-path-fanout.md)
- [Accepted runtime-integrity ADR](adr/0001-runtime-integrity-improvements.md)

### Planning

- [Current roadmap](roadmap.md) — a small navigation list of active follow-up
  work. It is not an implementation-state database.

## Archive

- `archive/superseded-specs/` contains replaced architecture and consumer
  specifications, including the former `SubagentProvider`-first model and the
  old coding-team implementation spec.
- `archive/implementation-plans/` contains plans whose implementation has
  landed or whose remaining work is tracked elsewhere.
- `archive/migration-reports/` contains completed cutover evidence and reports.
- `archive/roadmaps/` contains the retired large backlog roadmap.
- `archive/analyses/` contains investigations and proposals retained for
  historical context.
- `postmortems/` contains incident evidence and remediation history; it is not
  a runtime contract.

Historical documents remain intentionally unchanged where possible. Their
location is the lifecycle signal. If an archived document is useful for a
current change, verify every claim against code and tests first.

`docs/tmp/` is gitignored scratch space for local work and is not part of the
documentation authority chain. Do not link production guidance to files there.

## Document header

Every non-archived document should begin with these fields:

```text
Status: active | draft | deprecated | superseded | historical
Authority: normative | reference | guide
Verified-Commit: <commit or date>
Supersedes: <document or —>
Superseded-By: <document or —>
```

New code comments and active documents must use the canonical path of the
document they cite. Do not introduce a bare `spec.md §...` reference: the
repository has had several unrelated files with that name. Avoid machine-local
file-URI links and validation evidence that depends on a developer's home
directory.
