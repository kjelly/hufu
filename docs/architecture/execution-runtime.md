# Hufu execution runtime

> Status: active
> Authority: normative
> Verified-Commit: `72c20a5`
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

## Frozen task execution envelope

Every new task occurrence resolves an immutable `TaskExecutionEnvelope` before
leaf execution. The envelope binds four identities that must continue to agree
through retry, resume, direct-agent execution, declared steps, and backend
dispatch:

```text
task occurrence digest
    + ResolvedWorkerTools / LogicalToolsetDigest
    + EffectiveTaskResourceScope / ResourceScopeDigest
    + frozen execution target
```

The durable `TaskResourceScopeSnapshot` and
`DynamicToolAuthorizationSnapshot` are created before `task_created`. Restored
occurrences reuse those snapshots: current configuration may narrow or make a
frozen capability unavailable, but live resources and tools cannot widen an
existing occurrence. Cache and in-flight de-duplication use both the logical
toolset and resource-scope digests; the provider-visible schema digest is
telemetry, not cache authority.

## Resource-aware scheduling and path enforcement

Tasks declare typed resource claims with `read`, `write`, or `exclusive`
access. Workspace resources use canonical hierarchical names, so overlapping
ancestors and descendants conflict when either side can mutate. The DAG
scheduler may run disjoint bounded writers concurrently; malformed, unknown,
or unenforceable scopes fail closed or retain the conservative whole-root
claim instead of claiming unsafe parallelism.

For eligible local file tools, an authored artifact-backed workset can derive
bounded read/write paths. Those paths are installed from the frozen envelope
at every leaf entrypoint. On supported platforms the file operations use an
`os.Root`-anchored implementation, reject escapes and final symlinks, and
perform atomic writes with identity rechecks. Tools without a proven path
scope—including shell, terminal, custom, and MCP operations—remain
conservatively scoped.

## Stable dynamic MCP tool surface

`ResolvedWorkerTools` distinguishes the provider surface (`Tools` / `Names`)
from the logical authorization surface (`AuthorizedNames` /
`DynamicTargets`). On a new ordinary attempt, eligible manager-owned MCP tools
are represented to the model by one fixed-schema `use_dynamic_tool` gateway.
Agent command tools, schema-ineligible tools, closed tool sequences, and
result-repair/resume protocol surfaces stay direct.

The gateway's catalog is an immutable attempt-local projection of the frozen
snapshot. `search` and `inspect` disclose only that catalog. `call` validates a
closed JSON Schema subset locally, rechecks the exact logical target against
the coordinator policy and MCP `ToolAuthorizer`, verifies the live descriptor
fingerprint, and only then invokes transport. The provider schema therefore
stays stable while the logical digest changes with target inventory or schema.

Execution receipts retain bounded `ToolInvocationReceipt` entries containing
both gateway and logical identities. Transcript, audit, status, retry
evidence, last-operation tracking, and shadow projections use the logical MCP
target with the parent model call ID; a gateway call still consumes only one
model tool step.

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
7. A task attempt must carry the same frozen tool authorization, provider and
   logical digests, resource scope, and resource digest resolved for its
   occurrence. Missing or conflicting envelope state blocks execution before
   provider or tool transport.

## Effect snapshots and workspace versioning

The execution world's `WorkspaceSnapshotter` observes one external-provider
attempt: it hashes the workspace before and after the attempt so
`ValidateExecutionWorldDelta` can reject writes outside the writable roots.
Its snapshots live only in memory and it deliberately skips hufu bookkeeping
directory names. It is not a version store. Durable, restorable snapshots of
the subject root are the job of [workspace versioning](workspace-versioning.md),
which has its own inclusion policy and only checkpoints at run boundaries; a
rejected provider delta there becomes a `recovery_required` marker instead
of a snapshot.

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
  workspace boundary;
- [`coordinator_task_execution_envelope.go`](../../internal/team/coordinator_task_execution_envelope.go)
  — immutable per-attempt tool/resource binding;
- [`coordinator_resource_scope.go`](../../internal/team/coordinator_resource_scope.go)
  — frozen claims, path derivation, and resource digest;
- [`dynamic_tool_gateway.go`](../../internal/team/dynamic_tool_gateway.go) —
  stable MCP provider surface and logical dispatch;
- [`scoped_file_access_unix.go`](../../internal/tools/scoped_file_access_unix.go)
  — root-anchored bounded file access.
