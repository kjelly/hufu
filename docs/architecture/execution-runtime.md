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
ExecutionTarget + ExecutionTopology (+ ExecutionCandidates for a route)
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
  freezes its target and ordered topology before dispatch. A worker bound to
  an execution route also freezes the route's ordered candidates
  (`ExecutionCandidates`, carried in the occurrence's `execution_route`);
  candidate 0 is the `ExecutionTarget`, and the topology stays `[primary]`.
- `ExecutionRegistry` is the only worker-backend registry. It validates the
  target, rejects unknown backends, and prevents undeclared scheduler-side
  fallback: the only fallback is the deterministic route policy below.
- `ExecutionBackend` owns transport-specific execution. Hufu retains task
  lifecycle, tool authorization, verification, receipts, recovery, and final
  outcome ownership.
- `BackendBinding` records the session/attempt binding selected for the frozen
  target. The binding is evidence, not a permission to retarget the task.
  A task holds one backend session per branch, identified by backend and
  session ID; execution world, cwd, and turn are per-attempt diagnostics.
  Every attempt resumes that session, and a binding that names a different
  one fails with `ExecutionIdentityConflictError` instead of replacing it.
  The `backend_session_bound` event is durable before any turn starts and uses
  the per-task idempotency key `backend-session-bound:<task>`. An attempt
  whose projection has no binding reads the active branch lineage before it
  opens a session, and fails closed when that lineage cannot be read.
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

## Isolated worker workspaces

A worker agent (or a team default) can set `worker-workspace: {mode: isolated,
integrate: on-verified}`. The terms below are distinct:

- the **subject root** is the source project the team works on;
- the **control root** is Hufu's own directory for the project (session
  workspace, runtime workspace, and state); it never overlaps the subject
  root when isolation is enabled;
- an **attempt world** is a private copy of the subject root under
  `<control root>/attempt-worlds/<world id>/root`, and it is the attempt's
  **execution root**: the root its tools and verification resolve against;
- the **runtime workspace** (`<control root>/runtime`) and **workspace version
  snapshots** are unrelated to attempt worlds, and the user's **Git working
  tree** and `.git` are never written.

Admission freezes the policy on the occurrence. Only a `workspace_write`
task of an LLM-backend worker runs isolated; a read-only task of the same
agent runs shared. Team load rejects isolated agents that combine the mode
with extra-models, an external agent backend, MCP tools, a phase workflow,
`tools: all`, or the sudo, scp, lua, golang, or terminal tools, and task
admission rejects structured steps, actions, fan-out, and a non-managed
workspace. `request_agent` refuses isolated sub-agents, which it would run
inline in the subject root. An isolated setting is never silently downgraded:
a world that cannot be prepared (a nested repository, a special file, a
symlink out of the project) blocks the task as an environment failure.

Each dispatch attempt gets a new world with a random ID. Preparation copies
the managed files—including uncommitted and untracked ones, excluding ignored
files and `.git`—under a shared integration lock, and records a kind-aware
baseline manifest. The attempt's file tools resolve paths in the world and
refuse writes into the subject root or another world; shells start in the
world with `GIT_CEILING_DIRECTORIES` set. Verification runs in the world,
while verification fingerprints stay on the subject root so repeated-failure
detection keeps working. The subject root is not changed while the attempt
runs.

After verification and every other completion check pass, and before
`TaskDone`, the attempt's delta (new paths the project ignores are dropped)
is applied to the subject root under the exclusive integration lock. Every
path's precondition is checked before anything is written: the current state
must equal the attempt's baseline, or already equal its final state. Any
other state is a `workspace_conflict`: nothing is written, the world is
deleted, and the worker retries from the current subject root within its
retry budget. Writes use temp files and renames that never follow symlinks,
deletions run last, and the result is verified. An apply that fails part-way
is retried once and otherwise blocks the task (`workspace_apply_incomplete`,
`needs_human`) with the world kept. Because the apply precedes `TaskDone`,
downstream tasks see the changes. A failed attempt's world is discarded, so
its writes never reach the subject root or the next attempt.

Isolated writers hold a whole-root **read** claim (resource scope snapshot
version 2), so they run alongside each other and alongside readers, while
shared writers still hold the exclusive claim.

The lifecycle is durable: `attempt_workspace_prepared`,
`attempt_workspace_apply_started` (carrying the verified result,
verification, receipt, and coordinator output the task completes with),
`attempt_workspace_apply_conflicted`, `attempt_workspace_applied`,
`attempt_workspace_discarded`, and `attempt_workspace_orphan_removed`. The
world state is derived from the session's global events, not from a
branch-filtered view.

Every public entry point that can dispatch a worker (`Run`, direct agent,
targeted retry and reconcile, `ContinueWithPrompt`) reconciles attempt worlds
after the task journal is initialized and before any dispatch or resume
decision, whatever the execution profile:

- a world that was **applying** is applied again (the apply is re-entrant)
  and its task completes from the recorded evidence; if the apply cannot be
  proven, the task is blocked and the world kept;
- a world that was **applied** while its task is not done completes the task
  without re-running the worker or repeating verification (worker memory
  ingestion, STM, and reflexion are skipped);
- the latest **prepared** world of a `protocol_incomplete` task is kept for
  its result-only repair, which applies it before `TaskDone`; if that world
  is gone, the task is re-dispatched instead of completing from its repair
  provenance;
- any other world of the current branch is deleted, but a world that is
  applying, or applied while its task is not done, never is;
- a world of another session branch is only reported, and a directory
  without a valid owner marker is left alone.

Known limits of v1: a shell can still write the subject root through an
absolute path (bash has no path scope); a concurrent read-only task can
observe a multi-file apply half-way; and another Hufu process on the same
subject root is protected only by the per-path preconditions. If a crash
leaves the subject root mid-apply, the next admission records it as an
`external_drift` workspace version snapshot before recovery completes the
apply, exactly as for any interrupted run.

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
2. Dispatch resolves only a frozen target: the occurrence's
   `ExecutionTarget`, or, for a route-bound occurrence, the entry of its frozen
   `ExecutionCandidates` selected by the deterministic fallback policy. It
   never reconstructs a target from a current model flag, live provider
   configuration, or a coordinator's prose.
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

## Execution routes and fallback

`hufu.yaml` `execution-routes` names ordered lists of 1–4 backend-qualified
LLM targets; team.yaml and agent frontmatter reference a route by name. A
`-m` or `--worker-model` target makes the worker single-target and drops its
route. Admission freezes the route's name, digest, candidates, and
`fallback-on` on the occurrence, and the execution policy snapshot pins the
digest, so a changed route fails resume closed. A multi-candidate route cannot
be combined with extra-models, escalate, escalate-on-retry, or a decision-role
agent, and a coordinator cannot pick another model for such an agent.

A route-bound attempt that fails with a provider error may fall back to the
next candidate. The failure is classified structurally
(`ClassifyProviderError`): HTTP 429 is `rate_limited`; 500/502/503/504, a
refused or reset connection, DNS, and TLS failures are `provider_unavailable`;
404 (or a documented "model not found" body) is `model_unavailable`; a
transport timeout is `transport_timeout`; 401/403 (`auth_failed`) and context
overflow (`context_length_exceeded`) never fall back. A fallback happens only
when every check holds, in order:

1. the class is in the route's `fallback-on`, and the attempt's own context
   was neither timed out nor cancelled (task timeouts, verification,
   semantic, and policy failures are never provider failures);
2. a next candidate exists (at most `len(candidates)-1` fallbacks per
   dispatch);
3. nothing the attempt did would be repeated: its effective side effect is
   `none`; or it ran in an unapplied isolated world; or, for a shared
   `workspace_write`, every tool call the policy gate let through was
   read-only; or, for any other side effect, no call ran at all. The gate
   records each authorized call before the tool runs, because a failed model
   step drops its messages; without that record a fallback is denied;
4. the run's wall-clock and token budgets are not exhausted;
5. no cancellation was requested.

A fallback is the attempt loop's next round on the next candidate, recorded
by `execution_fallback_decided` before it starts. It does not consume the
retry budget, and it starts the candidate from scratch: no retry statistics,
reflection, or retry context. Backend selection, the backend semaphore, the
provider-bound context profile, and the provider model ID all follow the
candidate. A normal retry, a DAG retry, and a resumed dispatch always start on
the primary. The occurrence's frozen `ExecutionTarget` never changes: each
receipt records the target it ran on, its `candidate_index`, and on a fallback
attempt `fallback_from` and `fallback_failure_class`; an attempt whose
provider failure did not fall back records `fallback_denied_reason`.

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
- [`attempt_workspace_runtime.go`](../../internal/team/attempt_workspace_runtime.go),
  [`attempt_workspace_apply.go`](../../internal/team/attempt_workspace_apply.go), and
  [`attempt_workspace_recovery.go`](../../internal/team/attempt_workspace_recovery.go)
  — isolated attempt worlds, their apply, and their crash recovery;
- [`execution_route_admission.go`](../../internal/team/execution_route_admission.go),
  [`execution_fallback.go`](../../internal/team/execution_fallback.go), and
  [`provider_failure.go`](../../internal/team/provider_failure.go) — route
  binding, deterministic fallback, and provider failure classes;
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
