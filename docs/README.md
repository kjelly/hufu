# Hufu documentation map

> Status: active
> Authority: normative
> Verified-Commit: `4e40ed0`
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
- [DecisionPrimitive](architecture/decision-primitive.md) — bounded typed
  decisions, backend contracts, strict sidecar mapping, the System One
  decision-model adapter, the standalone `hufu decisionrt` CLI, native
  agent-team tools, and runtime control decisions.
- [Decision authoring UX](architecture/decision-authoring-ux.md) — canonical
  profile/request/routing schema, compatibility normalization, inspection,
  and dry-run migration contract.
- [Run outcome](architecture/run-outcome.md) — the policy implemented by
  `EvaluateRunOutcome` and `CompletionGate`.
- [Unified observability inspector](architecture/unified-observability-inspector.md)
  — read-only run, task, evidence, context, trace, and replay projections.
- [Operator experience](architecture/operator-experience.md) — implementation-ready
  scope, overview snapshot, deterministic next-action, CLI/TUI presentation,
  compatibility, and incremental delivery contract.
- [Model metadata](architecture/model-metadata.md) — model profile evidence,
  runtime introspection, and effective context admission.
- [Cost governance and portable team packages](architecture/cost-governance-and-portable-team-packages.md)
  — implementation-ready generation-cost admission, replayable projections,
  deterministic team packaging, inspection, and atomic local installation.
- [Workset](architecture/workset.md) — artifact-backed fan-out, immutable
  expansion receipts, typed results, and group verification.
- [Memory learning](architecture/memory-learning.md) — outcome attribution,
  replayable experience, and L3/L4 rollout boundaries.
- [Memory promotion](architecture/memory-promotion.md) — review-gated LTM,
  policy, and skill promotion.
- [Hybrid retrieval lineage scope](architecture/hybrid-retrieval-lineage-scope.md)
  — draft authorization design for exact, lexical, and vector retrieval; the
  current 100K workaround remains until the design is accepted.
- [Workspace versioning](architecture/workspace-versioning.md) — content-addressed
  snapshots of the subject root that session fork, checkout, and restore
  switch between; modes, locks, recovery, and GC.
- [SQLite maintenance policy](architecture/sqlite-maintenance-policy.md) —
  discovery evidence and the still-unmet authorization contract for any
  explicit maintenance operation.
- [Strict verification design](architecture/strict-verification.md) — a draft
  mechanism design and acceptance-case reference; code and tests remain
  authoritative for implementation status.

### Guides and references

- [Decision authoring](guides/decision-authoring.md)
- [Agent-team DecisionPrimitives](reference/decision-primitives.md) — trusted
  catalogs, explicit grants, bounded tool calls, and durable replay.
- [Runtime control decisions](reference/control-decisions.md) — off, shadow, and
  active `systemone` modes for the agent matcher, unattended `ask_user`, path
  reviewer, and guard reviewer, and how to read their evidence.
- [Read-only runtime inspection](guides/inspect.md)
- [Managed workspace migration](guides/workspace-migration.md)
- [Operator journeys](guides/operator-journeys.md)
- [Operator CLI migration](guides/operator-cli-migration.md)
- [Operator troubleshooting and recovery](guides/operator-troubleshooting.md)
- [Operator experience preview and fallback](guides/operator-experience-preview.md)
- [SSH tool](guides/ssh-tool.md)
- [Action providers](reference/action-providers.md) — team-owned command,
  embedded Go, and MCP tool adapters for structured runtime actions.
- [Canonical context SQLite schema](reference/context-sqlite-schema.md)
- [Operator experience Phase 0 baseline](reference/operator-phase0-baseline.md)
  — preserved CLI contracts, deterministic journey corpus, and usability
  measurement entrypoint.
- [Cost and team-package Phase 0 baseline](reference/cost-package-baseline.md)
  — verified generation-call, persistence, schema, and package-ownership
  boundaries used by the implementation-ready cost/package contract.
- [Generated operator command reference](reference/operator-command-reference.md)
- [Workspace command reference](reference/workspace-command-reference.md)
- [Workspace versions SQLite schema](reference/workspace-versions-sqlite-schema.md)
- [Operator release readiness](reference/operator-release-readiness.md)
- [Operator requirement traceability](reference/operator-requirement-traceability.md)
- [Operator usability evaluation](reference/operator-usability-evaluation.md)
- [Improvement artifact schemas](reference/improvement-artifact-schemas.md)
- [Agent definition format](reference/agent-format.md)
- [Skill discovery](reference/skill-discovery.md)
- [Execution compatibility sunset](deprecations/execution-compatibility.md)
- [Workset path fan-out deprecation](deprecations/workset-path-fanout.md)
- [Review scope template-variable bridge](deprecations/review-scope-template-var.md)
- [Accepted runtime-integrity ADR](adr/0001-runtime-integrity-improvements.md)

### Planning

- [Current roadmap](roadmap.md) — a small navigation list of active follow-up
  work. It is not an implementation-state database.

## Archive

- `archive/superseded-specs/` contains replaced architecture and consumer
  specifications, including the former `SubagentProvider`-first model and the
  old coding-team implementation spec.
- `archive/implementation-plans/` contains plans whose implementation has
  landed or whose remaining work is tracked elsewhere, including the
  [SQLite optimization implementation record](archive/implementation-plans/hufu-sqlite-optimization-plan.md),
  the [managed workspace registry implementation record](archive/implementation-plans/hufu-workspace-registry-improvement-plan.md),
  the [team action catalog implementation record](archive/implementation-plans/team-action-catalog.md),
  the [MCP-backed action provider implementation record](archive/implementation-plans/mcp-action-provider.md),
  the [DecisionPrimitive System One backend implementation record](archive/implementation-plans/decision-primitive-systemone-backend.md),
  the [DecisionPrimitive control decisions implementation record](archive/implementation-plans/decision-primitive-control-decisions.md),
  and the [learning runtime gap closure record](archive/implementation-plans/learning-runtime-gap-closure.md).
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

`bin/check-docs` checks lifecycle headers in active documents and relative
links in active and archived documents. For an implementation claim that names
a Go function, use a link labeled `Go function: FunctionName` to its `.go`
file; the checker also verifies that the function is declared there.
