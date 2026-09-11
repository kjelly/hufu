# Execution target migration report

> Status: historical
> Authority: reference
> Verified-Commit: `6ab9951`
> Supersedes: —
> Superseded-By: [execution runtime](../../architecture/execution-runtime.md)

This report records the execution-target migration that led to the current
[execution runtime](../../architecture/execution-runtime.md).
It is an implementation/status artifact, not a replacement for the
specification's mandatory regression matrix.

## Durable identity

New task occurrences freeze `ExecutionTarget` and `ExecutionTopology` before
`task_created`. The scheduler resolves only that frozen target through
`ExecutionRegistry`; a missing target is an admission failure and is never
rebuilt from `Model` or `SubagentProvider` at dispatch time.

During compatibility migration, `Model`, `ModelTopology`, and the binding
shadows remain readable for old workspaces. Target-backed lifecycle events
write `ExecutionTarget`/`ExecutionTopology` and `BackendBinding`; they omit the
retired `SubagentProvider` marker. Target-less historical task projections may
still carry that marker, and `ProviderBinding` remains a compatibility shadow
until the public compatibility sunset. `backend_session_bound` is the
canonical writer; `provider_session_bound` remains a replay reader.

## Resume migration

An unfinished legacy occurrence is migrated before dispatch. Evidence order is:

1. the occurrence's durable backend/provider binding;
2. a matching `model_profile_resolved` event in the task's run lineage;
3. an immutable configuration snapshot in that lineage, if a historical
   version ever wrote one.

This repository has no historical immutable provider-configuration snapshot
event, so source 3 has no current data source. Live configuration is not
accepted as evidence. A qualified legacy model without evidence fails with
the typed `LegacyExecutionTargetAmbiguousError` and no migration event is
appended.

## Backend semantics

`ollama` is the canonical built-in LLM backend. Historical `local` input
canonicalizes to `ollama`; new durable selectors and targets must use
`ollama`. Named LLM and agent backends share one namespace. A new
`backends.<name>` entry cannot coexist with either legacy namespace under the
same canonical key. `backends.codex` uses
presence-aware merging: omitted scalar fields inherit built-in settings while
`command` and `inherit-env` replace atomically.

Direct workers, coordinator-created tasks, todo-created tasks, and delegated
subtasks resolve canonical targets through `ExecutionRegistry`. Agent backends
do not pass through the OpenAI-compatible model-profile or tool-capability
lookup path.

## Phase 7 classification

The previous “provider identity and model identity are separate” formulation
is classification **(b)** from `spec.md` section 3: it is superseded by the
canonical `ExecutionTarget` identity. The durable no-retarget property remains
valid and is re-asserted through canonical admission and replay.

`TestDurableTaskModelDoesNotRetargetOnResume` and
`TestProviderBindingDoesNotRetargetAfterConfigChange` are retained as
compatibility coverage while legacy shadows exist; their assertion boundary is
the frozen `ExecutionTarget`, not a scheduler-side split resolver. The former
production `resolveSubagentProvider` function was deleted and its precedence
coverage was rewritten as `TestLegacyProviderCompatibilityPrecedenceAtCanonicalAdmission`.

| Remaining legacy reference | Classification |
| --- | --- |
| `ProviderManager` / `ModelRuntime.ProviderFor` | Required LLM adapter implementation detail |
| `HufuLocalSubagentProvider` | Private compatibility adapter behind `LLMExecutionBackend` |
| `SubagentProvider`, `ProviderBinding`, `subagent-providers` | Legacy read/config and dual-write compatibility |
| `ReduceToTodoList` | Diagnostic/test projection only; production resume uses `ReplayTodoList` |
| Target reconstruction from legacy state | Replay/migration compatibility only; not scheduler selection |

The remaining compatibility fields may only be deleted after old durable
workspaces and public configuration compatibility are formally retired.

## Validation evidence

Focused coverage includes canonical parsing/registry behavior, Codex
attempt/retry/direct dispatch, replay and shadow projection, migration
evidence, backend concurrency, target preflight, doctor/dry-run/status/report
output, role-model override isolation, projection parity, and transport-target
selection. The complete T01–T38 matrix is covered by the final full test and
race suites; vet, build, diff, and lint gates also pass.

## Phase reports

| Phase | Files / behavior | Durable compatibility | Focused evidence / remaining path |
| --- | --- | --- | --- |
| P1 | `internal/execution` adds typed selector, target, backend kind, and capabilities. | Object-form wire identity; `ollama` is canonical `local`. | Parser and target tests pass. |
| P2 | `internal/team/execution_{backend,registry}.go` adds registry and LLM/agent adapters. | No durable field deletion. | Registry and direct-LM capability tests pass. |
| P3 | Todo/event/checkpoint/shadow projections dual-write target, topology, and backend binding. | Checked replay rejects identity disagreement; legacy reducer reads remain. | Replay/parity tests pass. |
| P4 | Worker, direct, delegation, and retry dispatch resolve frozen targets through the registry. | Old provider adapters remain behind the registry. | Fake Codex app-server E2E/retry/direct tests pass. |
| P5 | `-m` is worker-only; role-specific targets, canonical backends, and preflight are added. | Legacy config is read and mapped; new teams use target keys. | CLI override, preflight, doctor, and no-mutation tests pass. |
| P6 | Legacy resume migration, session event, canonical backend concurrency and telemetry are added. | Old event/session readers remain; no live configuration is used as migration evidence. | Migration, replay, profile, concurrency, report, and status tests pass. |
| P7 | Legacy scheduler resolver deleted; compatibility conversion occurs only during admission/replay. | `SubagentProvider`/`ProviderBinding` remain temporary shadows for old workspaces. | Canonical-admission precedence and end-to-end regression tests pass; final legacy field retirement awaits compatibility sunset. |

All Go phases require the complete quality gate (`go test ./...`, race, vet,
build, and `golangci-lint run`) before release acceptance. The elevated final
validation completed successfully; logs are retained at
`/tmp/hufu-go-test-final2.log` and `/tmp/hufu-race-test-final.log`.
