# Hufu active roadmap

> Status: active
> Authority: reference
> Verified-Commit: `6ab9951`
> Supersedes: `archive/roadmaps/future-improvement-roadmap-2026-07.md`
> Superseded-By: —

This is a deliberately small navigation roadmap. It does not claim to be a
database of implementation state, and it must not override code, tests, or an
accepted ADR. Detailed historical plans remain in `archive/`; new work should
be tracked in an issue or ADR and linked here only while it is active.

## Active follow-up areas

| Area | Current boundary | Next useful work | Authority |
| --- | --- | --- | --- |
| Execution compatibility | New durable identity is `ExecutionTarget`; legacy `local`, provider fields, and provider bindings remain readable for old workspaces. | Define and test the public compatibility sunset before deleting legacy fields. | [execution runtime](architecture/execution-runtime.md) |
| Decision runtime | Capability-aware reference/JUDGE/CHALLENGE/REVISE routing and pinned bindings are implemented; outcome calibration remains data-gated. | Keep the deferred Phase 5 entry criteria and metrics honest as real runs accumulate. | [decision runtime](architecture/decision-runtime.md) |
| Model profiles | Runtime resolver, provider introspection, provenance, cache, and effective-context admission are implemented. | Maintain provider adapters and update tests when provider metadata contracts change. | [model metadata](architecture/model-metadata.md) |
| Worksets | Artifact-backed manifests and expansion receipts are canonical; path-based TSV fan-out is compatibility-only. | Complete the documented release-cycle migration and then remove the legacy path. | [workset](architecture/workset.md) |
| Memory learning | Canonical context, injection manifests, typed memory uses, outcome events, and promotion gates are runtime boundaries. | Treat ranking/consolidation changes as separately reviewed experiments with replay evidence. | [memory learning](architecture/memory-learning.md) |
| Documentation integrity | Active docs now have explicit lifecycle and authority metadata, with link/path checks in CI. | Extend the checker when new lifecycle rules or document classes are added. | [documentation map](README.md) |

## Recently completed documentation work

- Replaced the flat active-doc set with lifecycle directories.
- Promoted execution-target, run-outcome, workset, and memory boundaries to
  concise architecture references.
- Archived completed migration reports and implementation plans without
  deleting their historical evidence.
