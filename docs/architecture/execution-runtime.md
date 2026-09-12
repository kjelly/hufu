# Hufu execution runtime

> Status: active
> Authority: normative
> Verified-Commit: `6ab9951`
> Supersedes: `archive/superseded-specs/subagent-provider-runtime-spec.md`, `archive/superseded-specs/runtime-composability.md`
> Superseded-By: —

This document is the current execution abstraction. The former
`SubagentProvider`-first documents are historical design records; they are not
the scheduler's canonical identity.

## Canonical model

```text
ExecutionSelector
        │ resolve once
        ▼
ExecutionTarget + ExecutionTopology
        │ frozen before task_created
        ▼
ExecutionRegistry
        │ validates and selects
        ▼
ExecutionBackend / BackendBinding
        │ runs one authorized attempt
        ▼
AttemptRequest → AttemptResult → receipt / verification / lifecycle events
```

- `ExecutionSelector` is untrusted user or configuration input. A bare model
  is not durable identity until a configured default LLM backend resolves it.
- `ExecutionTarget` is the durable pair `{backend, model}`. A task occurrence
  freezes its target and ordered topology before dispatch.
- `ExecutionRegistry` is the only worker-backend registry. It validates the
  target, rejects unknown backends, and prevents a scheduler-side fallback to
  a different provider.
- `ExecutionBackend` owns transport-specific execution. Hufu retains task
  lifecycle, tool authorization, verification, receipts, recovery, and final
  outcome ownership.
- `BackendBinding` records the session/attempt binding selected for the frozen
  target. The binding is evidence, not a permission to retarget the task.
- `ExecutionWorld` is the Hufu-owned workspace/effect boundary for providers
  that can run native process or filesystem operations. Its capabilities do
  not grant authorization by themselves.

The implementation lives in [`internal/execution`](../../internal/execution),
the registry/backends in [`internal/team`](../../internal/team), and the
canonical attempt contract in `AttemptRequest`.

## Backend identity

`ollama` is the canonical built-in LLM backend name. `local` is accepted only
as a historical alias and canonicalizes to `ollama`:

```text
input:    local/qwen3:8b
durable:  ollama/qwen3:8b
```

The alias rule is intentionally narrow. It does not equate named LLM or agent
backends, and it does not change the model leaf. New configuration and newly
written durable events must use `ollama`; old workspaces may still contain
`local` or legacy provider fields while compatibility is retained.

## Admission, resume, and replay invariants

1. Resolve and validate the target before `task_created`; a missing backend or
   target is an admission failure.
2. Dispatch resolves only the frozen target. It never reconstructs a target
   from a current model flag, live provider configuration, or a coordinator's
   prose.
3. Resume migration uses durable backend/provider binding first, then matching
   historical model-profile evidence. Live configuration is not migration
   evidence. Ambiguous legacy identity fails closed.
4. Replay rejects disagreement between canonical target/topology and legacy
   shadow fields. It never silently retargets a task.
5. New lifecycle events write `ExecutionTarget`, `ExecutionTopology`, and
   `BackendBinding`. Current receipts write `backend`, policy snapshots write
   v4 `{model, backend, provider_key}` routes, and execution-event shadows
   write `backend`; `SubagentProvider`, `ProviderBinding`, receipt
   `subagent_provider`, and policy-route `legacy_provider` are compatibility
   readers/shadows only until the public compatibility sunset.
6. External execution providers return untrusted attempt results. Hufu
   canonicalizes the result and applies the same verification and acceptance
   gates used by local workers.

## Source of truth

For implementation details, use code and tests first:

- [`target.go`](../../internal/execution/target.go) — parsing and canonicalization;
- [`execution_registry.go`](../../internal/team/execution_registry.go) — target
  resolution and backend registration;
- [`execution_target_replay.go`](../../internal/team/execution_target_replay.go)
  — replay conflict checks;
- [`execution_target_migration.go`](../../internal/team/execution_target_migration.go)
  — legacy admission migration;
- [`execution_world.go`](../../internal/team/execution_world.go) — effect and
  workspace boundary.
