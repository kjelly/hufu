# Hufu execution runtime

> Status: active
> Authority: normative
> Verified-Commit: —（本次工作樹，尚未提交）
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
  A task holds one backend session per branch. Its identity, backend and
  session ID, never changes; attempt, execution world, cwd, and turn are
  execution details that may change between attempts.
  - The `backend_session_bound` event is durable before any turn starts and
    uses the per-task idempotency key `backend-session-bound:<task>`. It
    records the task's first binding, so its `attempt` and
    `execution_world_id` belong to that binding. Each later attempt's session
    and world are in its execution receipt and in the task transition's
    `backend_binding`.
  - The first event under that key is the anchor. Every later binding on the
    active lineage, in a session event or in a task transition, must name the
    anchor's identity. A history written before the per-task key has no
    anchor: its last binding wins, and the next bind writes the anchor.
  - Before an attempt starts its backend, it applies this rule to the active
    lineage: a projected binding must match the durable one, and an empty
    projection resumes the durable session rather than opening a new one.
    Startup replay does not enforce the rule, so `hufu reconcile` and
    `hufu retry` still reach a task whose binding conflicts.
  - A backend that resumes a different session fails the task as
    `session_resume_mismatch`. Contradictory or unverifiable identity evidence
    (a second session, another backend, a broken hash chain or lineage) fails
    it as `identity_conflict`. Both block the task for reconciliation
    (`ReconcileOnly`) and are never retried or replanned automatically. A
    storage I/O fault while reading or appending the binding is
    `environment`. A mismatch during result repair keeps the `protocol`
    disposition, because the turn has already changed the workspace.
- `ExecutionWorld` is the Hufu-owned workspace/effect boundary for providers
  that can run native process or filesystem operations. Its capabilities do
  not grant authorization by themselves.

The implementation lives in [`internal/execution`](../../internal/execution),
the registry/backends in [`internal/team`](../../internal/team), and the
canonical attempt contract in `AttemptRequest`.

## Required worker completion

An outcome team may declare `acceptance.required-workers` as a list of worker
names. Acceptance requires at least one completed model-worker task with an
explicit `status: success` typed result for each name. An empty run, a missing
stage, a failed or `completed_with_gaps` stage, or a runtime-owned action using
the same agent name cannot satisfy the check. A team whose stages can finish
their work while reporting an external limitation, such as a research team
that records an inaccessible source, may set
`acceptance.required-workers-accept-gaps: true`; a `completed_with_gaps`
result then also satisfies the check, while `partial`, `failed`, and `blocked`
still do not. Staged coding teams leave it unset. Team loading rejects
unknown, coordinator, and duplicate worker names. This check complements
`require-no-unresolved-tasks`; it proves the declared stages ran, while the
team's verifications and result contracts still define what their work must
achieve.

For a fixed first batch, `delegation.initial-batch` with `exact: true` also
enforces the complete ordered worker list before the first task is created.
The batch contract is durable across resume. A model-generated request
contract can be attached to an upstream resolver worker through
`result-contract`; dependent workers start only after that worker submits a
schema-validated successful result. The schema is pinned in the execution
policy snapshot and cannot be changed by a coordinator task payload.

New compatible local sessions use runtime result collection by default. Static
`result-transport: runtime|tool` in team configuration or agent frontmatter is
pinned in the execution policy; an agent override takes precedence over the
team default. Dynamic task JSON cannot change it. Historical policies without
the optional collection block retain tool transport and their original hash.
An implicit default preserves a closed sequence containing `submit_result`;
explicit runtime mode conflicts with that sequence and fails preflight.
External workers, native actions, auxiliary tasks and plan submission retain
their existing protocols.

Tool, collection and external proposal decoders reject duplicate object keys
and trailing JSON/text before interpreting claims. Their distinct canonical
owners, native evidence checks and shared completion expectations are documented
in [result transport contracts](../reference/result-transport-contracts.md).

The work phase has no `submit_result` authority in runtime mode. After work
returns, the runtime seals its complete report, transcript, source inputs and
original attempt identity before starting a separate `result_collection`
invocation on the admitted work target. It uses ordinary text generation with
zero tools, then strictly decodes JSON and calls shared result admission.
The accepted source is `collected`, with the same contract, evidence, verifier,
terminal and workspace integration gates as a tool submission. Runtime-owned
fields are still assembled from verified sources; the model cannot override them.
Declared string entries in `files_read` and `files_modified` are converted
losslessly to `{path: ...}` before DTO decoding. Duplicate keys, unknown fields
and unsupported shorthand remain invalid.

Each attempt has one collection call and at most one JSON/schema or
representation repair. Only two representation errors qualify. The first is a
failed `equals_projection` assertion (see Ordered result projections) whose
`pointer` is `/findings` or `/invariant_verification/assessments` and whose
reference array lies under `structured_payload`, when no other assertion fails
and the result declares no artifacts. The second is a violated invariant's
linked finding having a severity different from its verified definition. Any
other assertion or attestation failure stops collection. The collector receives
the original context's invariant IDs and severities. Repair may align the linked
finding's severity with the definition; the runtime never rewrites it. The
invariant's status and the other judgments stay unchanged, and the result must
still pass attestation.
Invariant summaries remain limited to 1000 runes; extended explanations belong
in `details`. The runtime does not truncate a claim to make it valid. A
turn is reserved durably before dispatch, so a lost in-flight call counts
against the two-turn limit. Provider failure, cancellation, an authorized-source
identity or integrity mismatch,
storage failure or exhausted repair budget stops result collection, retaining
work and isolated/fanout delivery artifacts. Resume uses received, prepared or
accepted durable bytes before considering another permitted model call. It
never redispatches completed work to repair a report. A storage continuation
adds no model turns. Collection usage, including cache counters, is accounted
separately from the frozen work usage.

Inspect derives content-free collection state, turns and failure reason from
receipts and checkpoint events. CLI JSON and reports expose these fields;
TUI step updates identify collection of the saved result. None of these
projections converts a blocked collection into task success. To select legacy
behavior, start a new session with `result-transport: tool`. Keep the newer
binary for sessions containing collection events; downgrading a binary or
editing the database is not a rollback procedure.

With `result-transport: tool`, result-only repair exposes only `submit_result` and requests a required tool
call from the LLM provider. This applies to initial repair, schema-only repair,
step-budget finalization and resumed repair. Provider compliance is still
checked: prose alone, an invalid payload or an incomplete result does not
complete the task.

Schema-only repair receives the complete rejected submission as fenced data,
including its findings and evidence references. The JSON is never cut at a
fixed character limit: model-context admission checks the complete repair
request and fails closed if it cannot fit. When the worker only produced
prose, the first result-repair turn's rejected submission supplies that data
to the second turn, including during resumed repair. Stream callbacks preserve
the arguments when returned steps omit them. Receipt prompt copies omit those
model-controlled arguments; repair does not add tools or replay execution.

Complete structured result payloads and their rendered final reports are bounded
at 2 MiB, including records copied by runtime evidence binding. The same limit
applies during assembly, validation, persistence replay, and evidence delivery;
an oversized aggregate fails preflight without dropping a source record.

Provider-facing result schemas retain portable fields, required properties and
enum values even when the full schema uses local `$defs` references or
unsupported conditional constraints. References are inlined within bounded
depth and node limits. The full pinned schema remains authoritative; constraints
omitted from provider guidance are still enforced on submission. Cyclic or
unresolvable shapes fall back to an open payload property with the contract
identity and full schema instructions.

Repeated `submit_result` argument rejections, including portable-schema
preflight and DTO decoding, share a stable failure category. After three
rejections, the worker stream stops and enters the existing bounded result-only
repair path. Changing JSON paths or call IDs cannot bypass that limit, and
result-only repair does not replay worker tools.

Result-only repair validates tool-backed citations against the original
attempt's observations through a read-only context, without appending repair
activity to its transcript. Resumed repair reads only the content-addressed
transcript pinned by the original receipt, verifies its digest, frozen input
snapshot and result schema, and retains the original execution attempt number.
Tool-evidence repair receives the complete retained read-only observations
(bounded by the transcript's 16 MiB evidence limit), including later pages and
quotes past a page's navigation. Context admission fails closed if this source
text cannot fit; it is not silently clipped to a prefix. Other repairs retain
bounded excerpts. Citation binding uses the full verified evidence, and stored
receipts and payloads retain their exact hashes. A checkpoint with authenticated
observations can be repaired even when its last assistant message is empty.

A tool-evidence contract with `side_effect: none` may use one final result-only
turn if its first repair stream fails or returns no `submit_result` call after
successful read-only observations. Live and resumed repairs normally share a
two-turn limit. If the first turn produced no submission or failed preparation
and the second first reaches a schema rejection, exactly one additional
schema-only correction is permitted. Two missing calls, repeated schema errors,
and a failed third turn cannot extend that budget. Resumed repairs checkpoint
each generation's diagnostic and used budget before the next call, and restore
the last rejection when continuing a schema correction. Exhausted repair requires
reconciliation with the original receipt, rather than a new worker attempt
submitting from history without its original evidence. The separate bounded
capability fallback for tasks without a tool-evidence contract still requires
proof that all recorded work tools were observations and starts a clean worker
context. Tool-evidence contracts, unknown tools and mutating tools never
authorize that fallback.

Request acceptance provides requirement-specific eligible source IDs. Observed
requirements accept only observed sources; response requirements must cite the
delivered candidate response. A mixed-kind proof is rejected even if it also
contains valid observations. One proof-repair turn may remove ineligible sources
or select existing quotations, but cannot change the frozen requirements or
upgrade a negative verdict. A still-invalid repair persists its validation error.

A blocked task stops new delegation while its occurrence or reliability scope
remains unresolved. If the original permitted retry later completes and its
reliability scope clears, Hufu releases that task-derived stop. Other blocked
occurrences and explicit operator, budget, or workflow wrap-up requests still
prevent delegation. Successful recovery never erases the earlier failure
receipt or bypasses the final acceptance checks.

An explicit graceful wrap-up request lets active workers finish their result,
receipt, and verification steps. Once they are terminal, an unfinished phase
is recorded as `CANCELLED` and the workflow enters `FAILED`; the run preserves
completed task evidence and ends as `cancelled` without another coordinator
model turn or an acceptance command for the incomplete workflow. A completed
VERIFY phase still follows the normal finish and acceptance gates. Budget and
task-derived stops retain their own dispositions. A cancelled workflow remains
terminal when its checkpoint is resumed; it cannot silently restart work.

Argument errors retain the rejected field's full path. When a full tool
example exceeds 2 KiB, feedback uses the rejected schema's allowed fields and
a bounded local example instead of repeating an entire fixed collection.
The local example is explicitly not a complete submission.
For a union with exactly one branch matching the submitted JSON type, an
invalid value reports that branch's rejected member (for example
`files_read[0].path`), rather than only reporting an invalid object. This
improves repair feedback without accepting an invalid union alternative.

Schemas can declare `x-hufu-fixed-collections` to separate known membership
from model judgment. The runtime builds source-hash-bound row keys from accepted
evidence and supplies a required manifest to workers and result-only repairs.
Each manifest includes a `submission_pointer` rooted at `/structured_payload`;
the collection's `items_pointer` is relative to that payload, not to the outer
`submit_result` arguments.
Workers submit keyed decision objects with required finite string enums; the
runtime generates the array, order, identities and copied values before schema
validation and receipt hashing. Unknown, missing or duplicate keys and fixed
field overrides are rejected within the attempt. Runtime-owned payload metadata
pins source hashes for commit, dependency, replay and report validation. This
uses the existing worker protocol and policy gates without semantic keyword parsing.
Without `decision_groups`, workers continue submitting their enum values. With
that opt-in extension, `decision_primitive` binds a finite choice to the durable
source/row/field and the runtime injects the settled values; result-only repair
does not infer missing choices. Runtime request/state choices use the same
catalog and started/settled/reused journal, with bounded immutable input snapshots.
`decision-runtime` supplies inference defaults independently of general sidecar
generation. `no-net` restricts tools/subprocesses, not trusted model or System One
inference; denied tools, phase policy, budgets and force-mcp still apply.
See [control decisions](../reference/control-decisions.md) and the
[fixed collection contract](../reference/agent-format.md#固定集合與有限選項決策).

A task's explicit `on_failure` edge owns its failure routing before generic
criterion retry. Its failure-class allowlist, repair limit, replay policy and
failure-signature checks remain authoritative even when an acceptance
criterion is still failed. A denied or exhausted edge cannot fall back to
criterion routing. Transient action-provider retries retain their existing
same-task policy. Successful tasks that made no criterion progress can still
route bounded criterion remediation.

When either failure route selects another task, the source's canonical failure
class, attempt, findings and verification diagnostic are redacted and committed
with the target's reset. A pending repair receives the same context through a
task transition without consuming a retry. An append failure publishes neither
the reset nor the new context. SQLite replay restores the context and worker
dispatch includes it in the required failure-context input. Criterion routing
cannot restart a terminal task with its own failure edge, and counts retries
already consumed by other routes or restored from the durable occurrence.

Blocking acceptance self-healing respects unresolved task recovery
dispositions. A task requiring `reconcile_only` or `needs_human` prevents
acceptance from reopening worker dispatch, even when the task's side-effect
class is `none`. An acknowledged finish ends with failed acceptance and the
original unresolved task; it cannot create a fresh task to bypass that
disposition. A runtime-verified resolution removes that blocker. Tasks allowing
`retry_worker` or `replan_required` retain the existing bounded self-healing
window.

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
`DynamicToolAuthorizationSnapshot` are created before `task_created`. The
dynamic snapshot freezes two things:

- the manager MCP targets, by name and descriptor digest;
- the `StaticToolCeiling`: the built-in and agent-scoped MCP tool names the
  worker's configuration grants.

The ceiling is phase-free. The phase gate hides execution tools outside
EXECUTE on every resolution, but it is not frozen.

Restored occurrences reuse those snapshots. Every resolution applies them:
each attempt, retry, result repair, resume, extra-model leaf, route fallback,
and declared structured step. Current configuration may narrow a frozen
capability or make it unavailable, but live resources and tools cannot widen
an existing occurrence.

An extra-model leaf retains the parent's frozen agent definitions and binds
only its indexed admitted target. It cannot rewrite the parent topology or
refreeze policy to admit another model. Parent policy validation precedes
leaf profile discovery and actual provider admission checks it again. Each
leaf has private task lifecycle state; the parent owns the single durable
occurrence lifecycle, and receives each leaf's context/memory manifests before
provider dispatch and receipts before completing the occurrence. Model
execution identity separates parallel manifests and retries, while outcome
credit remains capped across the parent occurrence. Provider token accounting
charges each leaf once; parent lifecycle telemetry aggregates receipt usage
without charging it again.

Tool schema normalization also owns its input. Admission serialization and
Fantasy receive fresh nested schema containers, preserving the raw tool
definition used for authorization digests across parallel workers.

- A tool added to an agent's `tools:` after admission is ignored for that
  occurrence.
- A tool removed or newly denied is dropped.
- Either difference is recorded once as a content-free
  `static_tool_grant_narrowed` event.

`hufu retry --task` reuses the same occurrence, so it keeps the frozen ceiling.
To run with a newly granted tool, start a new occurrence: replan or start a new
run. Snapshots from before the ceiling existed (version 1) are rejected, so
such occurrences cannot be resumed or retried.

Cache and in-flight de-duplication use both the logical toolset and
resource-scope digests; the provider-visible schema digest is telemetry, not
cache authority.

## Resource-aware scheduling and path enforcement

Tasks declare typed resource claims with `read`, `write`, or `exclusive`
access. Workspace resources use canonical hierarchical names, so overlapping
ancestors and descendants conflict when either side can mutate. The DAG
scheduler may run disjoint bounded writers concurrently; malformed, unknown,
or unenforceable scopes fail closed or retain the conservative whole-root
claim instead of claiming unsafe parallelism.

Mutations in one batch whose claims conflict run in batch order. The runtime
records that order on each task as `order_after`, separate from `depends_on`.
A task waits until every task in its `order_after` has finished, whatever the
outcome. Only a declared `depends_on` blocks a task when its producer fails or
is blocked.

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

The canonical `task_protocol_incomplete` event records a recoverable result
checkpoint and remains available for inspection and crash recovery. The legacy
execution-event compatibility export omits that checkpoint: it records the
subsequent verification/completion or terminal failed/blocked/cancelled outcome.
A successfully repaired submission therefore does not acquire a terminal error
in the compatibility stream.

Each completed result-repair generation also records its provider finish reason,
token usage, text/reasoning byte counts and tool-call count in the attempt's
`repair_provenance.history[].generation`. The diagnostic contains no generated
reasoning or duplicate source content. Legacy receipts and failures before a
completed generation omit it; an observed generation with no provider finish
reason records `unknown`. A `no_tool_call` classification alone therefore does
not imply that the model returned prose or exhausted its output allowance.
Numeric reasoning-token usage survives redaction. Readers accept the historical
`"[REDACTED]"` reasoning-token marker as unavailable/zero telemetry while
preserving the remaining diagnostic; other invalid string counts are rejected.

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

New public invocations bind a canonical run-input snapshot. A fresh input
context persists an empty snapshot even for teams that declare no inputs;
later no-input turns retain that exact snapshot and record its original run,
so completed sources keep their identity. Task canonicalization binds
that snapshot before decision admission for ordinary workers, actions (with or
without payload input bindings), sidecars, direct invocations, delegated
subagents and agent-created TODOs. Task creation and execution admission reject
missing, partial or different bindings rather than silently replacing them.
The same identity propagates into execution receipts and collection checkpoints
through the ordinary occurrence projection.
Offline audit validates ordinary invocation-bound worker receipts independently
of action materialization and optional request-acceptance contracts. Ordinary
replay and follow-up turns of legacy no-input contexts retain their existing unbound form; they do not
create a snapshot for historical work or authorize result-collection recovery.

Historical checkpoints missing this binding remain readable but cannot be
automatically rebound for recovery. Even when the run has a valid snapshot,
that snapshot alone does not authenticate an unbound task's sealed work or
accepted source receipts. Recovery diagnostics distinguish a missing task
binding from a missing snapshot; passing new inputs cannot supply historical
proof. No snapshot or receipt is synthesized from present-day task prose.

`hufu resume --branch <existing-branch> [--run <source-run>]` selects the exact
canonical event lineage before loading the coordinator. It refuses missing or
empty sources, never falls back to main, and atomically retains the outgoing
branch when activating the selected source. Before any model execution, it
checks the restored results against replay, their frozen inputs, pinned result
contracts, original receipts and content-addressed transcript bytes. A refusal
names available branches for read-only inspection; it does not start a fresh
workflow or regenerate accepted payloads.

For a blocked protocol submission, `resume --branch <source> --repair-result
<task-id>` explicitly grants two additional result-only turns. The grant and
previous repair history are durable, the original execution run/attempt,
transcript and input hashes stay unchanged, and only `submit_result` is exposed.
It cannot replay the worker or recover an isolated workspace. Completed
dependencies are handed off through their existing `evidence_from` IDs.
For runtime result collection on shared local workspaces, the same explicit
repair command first reassesses the sealed work and original claims. The old
failure reason, including a historical `source_invalid` misclassification, is
not sufficient authority by itself. The evaluator verifies the canonical task,
input snapshot, policy, source payloads, transcript, CAS bytes and settled
decisions before classifying `commit_local`, `repair_claims`,
`resume_collection`, or `blocked`. Source checks precede model-format checks;
a simultaneous source failure cannot be repaired as formatting.

`resume --branch <source> --run <source-run> --repair-result <task-id>
--dry-run --output json` provides a read-only assessment before branch
activation. It does not open a provider transport, invoke tools, infer decisions,
write diagnostics or alter active session state. It reports the original reason,
checks, task/branch/source-run identity, checkpoint references, fingerprint,
consumed/remaining turns, proposed additional allowance and accepted-task count.
Unknown counts and turns are omitted from JSON and shown as unknown in text. Preview cannot be combined with
`--repair-model`, `--report`, `--tui`, or new input assignments. A blocked preview
is a successful inspection with `disposition: blocked`; no grant is created.

Execution recomputes the assessment and atomically appends it with the grant and
submission-only transition, fenced against the journal head. A competing writer
must reassess rather than use stale authority. Valid durable claims receive no
new model allowance; invalid Prepared claims can receive up to two collector
turns. Consumed turns never decrease, and duplicate grants do not accumulate
allowance. Historical failure events and old prepared/response CAS references
remain available through the saved assessment and event lineage. Replay validates
the saved grant binding, not a new decision from current sources. Collector
repairs retain the original target, settled verdicts and complete source payloads;
source limitations remain in their source records and result summary.

This recovery path excludes isolated and fanout work, and cannot change policy,
contracts or execution targets. Source-dependent payloads with unverifiable shape,
missing frozen evidence, any nonempty assumption checks or memory-attribution
claims, active decision disciplines, paused checkpoints, and uncertain side effects
remain blocked with diagnostics. Unsettled terminal processes and the latest failed
terminal command in the original run require reconciliation before recovery.
Preview binds sidecar decision model identities without creating a provider or
rerunning settled decisions. A local result commit still
passes ordinary verification and workflow acceptance; repairing one task can
unlock undispatched downstream work and does not complete the entire run.

An optional `--repair-model <backend/model>` selects an auxiliary submitter
already present in both the original and current frozen LLM routes. The grant
and repair turns record that model separately; the original worker target and
execution receipt identity are retained. Unknown, bare and agent-backend repair
targets fail before generation. This lets an operator repair a submission with
an already admitted model when the worker model repeatedly exhausts its output
budget without a tool call.

An unused external agent backend's inherited environment does not block an LLM
continuation. This compatibility applies only when neither frozen nor current
model routes name that backend, and all other policy fields still match. The
runtime denies dispatch and execution-world access to such an unrouted backend;
environment changes for an active backend still fail admission.

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

## Ollama Cloud quota waits

Coordinator-managed model calls automatically wait after an Ollama Cloud HTTP
429 that rejected the request before any response was observed. This covers
coordinator, worker, direct-agent, extra-model and sidecar calls through the
shared provider admission boundary. Other providers retain their existing
retry/fallback behavior. Standalone native System One decision transports and
commands that do not use coordinator admission retain their own error policy.

Hufu queries the official [balance API](https://docs.ollama.com/api/balance).
For legacy plans it waits for the latest `resets_at` among exhausted session
and weekly limits. Monthly credit plans use `included.period.until` when both
included and purchased credits are exhausted. Hufu checks the balance again
at that deadline before continuing. Direct `https://ollama.com` providers use
their configured API key. A loopback Ollama daemon forwarding a `:cloud` model
uses `OLLAMA_API_KEY` for the balance query; this key must belong to the same
account as the daemon's cloud sign-in. Local-provider credentials are never
forwarded to the cloud balance endpoint.

Without a usable key or reset time, Hufu permits one generation probe every
**10 minutes**, shared across models using the same account. A longer
`Retry-After` delays the next probe further. Concurrent agents share the
cooldown and cannot issue simultaneous fallback probes. Balance requests have
a short independent timeout, a bounded response size, and never follow redirects.

Each rejected generation settles its provider invocation and releases its
provider slot before waiting. A resend gets a new invocation ID, passes
admission again, and retains the exact step input and previous tool results.
It does not replay tools, restart the agent, consume a task retry, or move to
a route fallback while that Ollama request is waiting. A partial response or
midstream failure is never automatically replayed by this policy.

Quota waits pause model working-time budgets, including enclosing coordinator
budgets, and are recognized by the silent-stall watchdog. Explicit parent
deadlines, the run's total duration/cost/token budgets and cancellation remain
authoritative; absent those limits, waiting continues until service recovers
or the operator cancels. Existing conservative cost settlement still applies
when a rejected invocation reports no usage.

The branch-scoped `provider_quota_cooldown` journal event stores only an opaque
account digest and the next check time. Restarted execution restores that
deadline; credential changes get a different identity. Journal failures stop
recovery before another request is sent. This restores the cooldown for work
the normal recovery policy resumes; it does not reopen an already failed run
or replay completed tasks. CLI/TUI status shows the next check time, and
runtime event exports preserve the wait history.

## Interrupted-task reconciliation evidence

Normal crash resume and operator-requested `hufu reconcile --task` use the
same typed reconciliation result. A declared `verify_spec`, `reconcile_tool`,
or `verify` check classifies exit 0 as `complete`, exit 1 as `not_started`,
exit 2 as `partial`, and any other exit, timeout, launch failure, or absent
result as `unknown`. Without a declared check, non-empty task output may
preserve `complete` only for `none` or `workspace_write` side effects. It is
never completion proof for `external_write`, `infra_mutation`,
`credential_mutation`, or `unknown` side effects; those remain `unknown` and
cannot be automatically replayed merely because output exists.

Before a reconciliation changes the task checkpoint or status, a
`recovery_decision` event records `reconcile_observed` with the persisted
recovery state, a fixed evidence source (`verify_spec`, `reconcile_tool`,
`verify`, `task_output`, or `none`), the task attempt, and an optional exit
code. The event contains no check command, task output, tool arguments, or
error text. `hufu inspect task` displays that evidence only for the selected
session, branch lineage, run, task, and attempt; historical events without a
source display `unknown`. The source is diagnostic evidence, not permission
to retry, bypass acceptance, or change the frozen execution target. A
reconciled `complete` still requires affected acceptance criteria to be
revalidated before the task becomes done.

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

### Read-only verification actions

Static workflow actions and catalog actions with `side_effect: none` may run
in PREPARE or VERIFY, as well as EXECUTE. VERIFY providers can inspect accepted
evidence after worker verification without reopening mutation authority. AUDIT
still forbids actions, and actions that can mutate state remain confined to
EXECUTE. Configuration validation, catalog discovery, and provider dispatch use
the same phase rule. A declared `task_result_assert` on an action evaluates the
provider's canonical runtime outputs before publishing the successful result or
receipt; verification failure leaves the occurrence failed.

### Ordered result projections

Teams may declare an `equals_projection` task-result assertion to compare two
ordered object arrays before a successful worker result is committed and again
at objective verification. The assertion's `pointer` selects the first array;
its configuration-owned `value` contains a reference `pointer`, 1–16 unique
`fields`, optional `optional_fields`, `field_defaults`, and optional boolean `allow_missing`.
Required and optional field names must be distinct, with at most 16 total. Both arrays must have the same
length, and every selected field must exist and match at the same index.
Fields listed in `optional_fields` may be absent from both objects; a value
present on one side must match on the other. An empty array is equivalent to
an omitted optional field because canonical DTOs may omit empty arrays. This
does not equate omitted values with null, false, scalar zero, empty strings,
or nonempty arrays. Additional fields are ignored; array order is significant. This supports an
outer result summary paired with a richer schema-defined record without
teaching the runtime domain-specific field names.

`field_defaults` may declare a JSON fallback for a selected field. During
comparison only, an omitted or null field uses that fallback on either side;
the submitted and stored results remain unchanged. For optional references and
evidence arrays, `{finding_index: null, missing_evidence: []}` treats omitted
references as null and omitted/empty evidence lists alike. Zero remains a real
index, nonempty evidence must still match in order, and numeric strings remain
distinct from numbers. Defaults for unselected fields fail contract validation.
Without this explicit policy, selected fields remain required on both sides.

`allow_missing: true` permits an omitted first array only when the reference
array exists and is empty, accommodating canonical `omitempty` fields. Null,
wrong types, missing references, extra entries, and substituted fields fail
closed. The policy is bounded, pinned in the execution contract and verification
fingerprint, preserved on resume, and reused during result repair. References
to runtime-owned transcript fields are deferred until transcript finalization.
Rejected submissions leave the original occurrence available for correction;
they do not persist an accepted result that later synthesis must repair.

Process-wide secret learning excludes source identifiers made of letters and
underscores, including public environment variable names. Such values remain
masked beside credential keys, and explicitly registered secret values remain
masked everywhere. Treating a public variable name as a learned secret would
otherwise rewrite earlier validated payloads during subsequent checkpoints,
invalidate their hashes, and change task event idempotency keys. Markdown
backticks are treated as quoting for learning, so real quoted credentials still
receive bare-value protection while quoted source symbols remain identifiers.
Public policy labels (`fail-closed`, `fail-open`, `deny-by-default`, and
`allow-by-default`) likewise remain ordinary text during bare-value learning.
For example, a review note saying `Missing credential: fail-closed` must not
rewrite earlier evidence of that policy. The keyed value is still masked;
explicit credential registration takes precedence over this learning guard.

Coordinator policy repair restricts dispatch only while a correction is
pending. A successful delegation clears that state; cumulative attempt and
success counters remain for telemetry and the bounded repair budget. Later
workflow steps use normal team delegation policy, including any explicit
`no-redispatch-after-success` restrictions. A historical correction must not
permanently consume every worker role used in the repaired batch.

Once `run_finished` is committed, normal completion does not rewrite unresolved
tasks as skipped. The sealed terminal snapshot keeps their states and original
diagnostics for inspection and recovery, without appending task transitions
after the run boundary.

### Selected dependency-result presentation

Static contracts can opt into `dependency-result-view: {mode: selected,
fields: [...]}` for ordinary workers with same-batch dependencies. The setting
is frozen in the admitted task and execution policy, rather than accepted from
model tool arguments. Full mode keeps the legacy formatter and identities.
See [the configuration reference](../reference/agent-format.md#靜態依賴結果視圖dependency-result-view).

The runtime resolves complete accepted dependency results through the existing
workset restrictions, then authenticates their completion events against the
active branch lineage and fork cutoff. A separate renderer produces selected
fields without modifying canonical results, receipts, CAS objects or access
grants. Structured upstream payloads and downstream complete-evidence or
result-contract requirements are incompatible with selected presentation.
Selected context remains required, uncompressed and complete; insufficient
capacity stops the downstream request before worker invocation.

Selected manifests use schema 4, including when composed with query attribution;
full manifests retain schema 2 or query schema 3. Each selected item binds the
normalized policy SHA-256, ordered source branch/run/task/attempt/revision,
completion event ID/hash and SHA-256 of each authorized full handoff. Its
`ContentHash` is the full SHA-256 of the actual selected context bytes. These
presentation hashes never replace canonical payload or receipt hashes.

Primary selected requests begin with `ModelCalled: false`, outcome `prepared`.
At the actual agent/provider invocation boundary the runtime persists the
dispatched manifest before invoking the worker; this denotes runtime dispatch,
not remote acknowledgement. Preview, cancellation and uncalled requests cannot
prove delivery. Retry, fanout, completion and result-only recovery verify the
frozen policy, exact source occurrence and dispatched view. Result-only recovery
does not rerun work to obtain omitted fields. Inspector and report projections
expose content-free attribution and distinguish preparation from dispatch.

### Follow-up workflows in one conversation

A completed workflow is one execution cycle, not the end of its conversation.
An additional user request through `run` or `chat` starts a new cycle in the
same session and branch. Conversation history, prior reports, immutable policy,
and historical results remain available; active tasks, phase results, retry
state, continuation budgets, and criterion evidence start fresh. Prior-cycle
results and verification receipts cannot satisfy the new cycle's required
tasks or verification. A schema explicitly opting into historical tool evidence
may use previously observed source text for a quotation in a new result; this
does not reuse an old task completion or fulfill a requirement for a fresh
source call. Teams requiring new independent verification keep a fresh-only
result contract and verify the new source calls.

The event-first `workflow_cycle_started` boundary includes the redacted request
and the previous task ID high-water mark. Replay restores that request even if
the process stops before checkpointing, and new task IDs never alias previous
receipts or grants. Historical run inspection still reads the original events.
Task journal history is retained, but its cache entries cannot repopulate a
later cycle; recovery reuses that cycle's own canonical tasks instead.

Workers listed in `delegation.share-request-with` also receive the complete
preceding workflow response on follow-up dispatch, retries, and recovery. The
runtime reads that response from the active branch's event lineage before the
cycle boundary, preferring `run_finished.response` and using the final assistant
message for older sessions. It does not substitute a task-progress summary or
a prior session's archived memory. The report is fenced historical reference,
never instructions, current evidence, or completion credit. It is required and
cannot be truncated: insufficient model capacity fails context preflight before
dispatch. Explicit profiles disabling historical memory still withhold it.
The context manifest records `previous_response:<event-id>`, its event source,
and the hash of the complete injected fragment. `inspect context --show-content`
resolves and verifies this reference without depending on `context.sqlite`.

Interrupted and failed workflows keep their existing recovery behavior.
`resume` never starts a new cycle, and dry-run and direct-agent entry points do
not implicitly reopen completed workflows. Execution-policy compatibility is
still enforced across the session. `--new` remains the explicit way to start a
separate conversation or adopt incompatible execution settings.

A failed workflow refuses a new request before input extraction. To retain a
read-only attempt's completed research and sealed tool evidence, fork the session
at its `protocol_incomplete` checkpoint before finalization failed, then resume
that branch with compatible execution settings. The original failed branch and
its history remain available; a metadata-only fork leaves project files alone.
Forking another branch never copies the outgoing branch's live tasks into the
child. Once the selected lineage contains events, replay determines its tasks,
including an empty task list at a point before any task was created. Only an
eventless legacy branch may use its own stored task-plan snapshot.
Recovered terminal results retain the original frozen input snapshot and record
its `run_input_origin_run_id`, verified against the selected branch lineage.
A new request that fails before input admission does not inherit old inputs.
If such a request left an unconfirmed terminal append, restart may record it as
failed only when its canonical history contains an aborted zero-turn checkpoint
and no model, tool, task or unknown activity. Other uncertain commits remain
blocked until their actual terminal event can be confirmed.

Recovery settles completed phase contracts through the same workflow observer
as normal execution before building the coordinator's phase-scoped tools. This
also covers a checkpoint taken after task completion but before phase settlement,
even when no task needs replay. The resulting workflow snapshot is persisted as
a canonical event and checkpoint; pending or operator-blocked tasks do not
advance, and objective VERIFY evidence and phase order remain required.
The closed context-purpose registry permits terminal-phase context only for
coordinator delivery, compaction, final-summary repair and acceptance review,
with the exact declared role and trigger. Context manifests keep `DONE` or
`FAILED`; this does not reopen dispatch or admit a new failed-workflow request.

The evidence manifest can retain completed executions from earlier runs in the
selected branch when their canonical task, successful receipt, result and frozen
input snapshot match the admitted recovery request. Its recovery snapshot ID
and hash bind that reuse; each execution binding retains the original run,
producer, attempt and transcript. Artifact occurrences remain attached to that
execution, and immutable artifact metadata and receipts are not rewritten.
Terminal validation and independent audit compare the recovery snapshot with
the admitted inputs and re-derive the original winning receipt. A loaded session
without input admission, another branch, changed result or mismatched receipt
cannot authorize reuse.

Independent audit validates a projected branch against the complete physical
event journal when visible neighbors are separated by events from other
branches. It requires the exact canonical branch projection, preserving deletion
and tampering detection without treating legitimate branch gaps as broken links.

After interrupted-task recovery, if every unresolved task is blocked with
`needs_human`, resume seals the existing unresolved result without starting the
coordinator model. Completed results, failed-task evidence and the required
operator action remain in the canonical result. Pending work, worker retries,
replanning and reconciliation retain their existing continuation behavior.

Coordinator streams retain the first hard tool failure independently of the
provider library's tool-result handling. A fatal error or exhausted policy
repair stops later tool dispatches and model requests, even when Fantasy turns
the error into a tool result and ignores the result callback's error. The failed
tool observation remains in history and the original runtime sentinel reaches
terminal finalization. Recoverable tool-error responses retain their bounded
correction path; worker streams do not inherit a coordinator's hard-stop state.

### Bounded artifact search

The built-in view tool accepts search_text for authorized artifact_ref inputs.
It searches exact literal UTF-8 text and returns absolute byte offsets plus
small excerpts, enabling focused follow-up byte reads. max_matches defaults to
1 (maximum 8), context_bytes defaults to 256 bytes on each side (maximum 1024),
and max_scan_bytes bounds the scanned window (1024 to 4194304 bytes, default
4194304). byte_offset chooses its start; search does not accept file_path,
line offset/limit or byte_limit. Scans preserve overlap at continuation windows
so literals crossing a boundary are not skipped. Excerpts preserve UTF-8, and
the total response respects both an 8 KiB cap and the attempt's artifact read
output budget. Incomplete scans explicitly identify unseen content.

Authorization uses the same artifact opener, policy gate and trusted read
metadata as ordinary view. Only returned excerpts enter decision observations;
a search is navigation and evidence for those excerpts, never a claim that the
model observed all scanned source. Denied, cancelled and invalid-text searches
issue no successful-read evidence. The mechanism interprets no domain-specific
snapshot format or task language.

Bound-decision observation publication also admits the exact encoded context
before exposing a new artifact response. The check uses every source row and
its declared decision-group limit, including the finding, prior reads and
JSON metadata. A capacity denial withholds the entire new response and its
read authority, without publishing an observation or poisoning the earlier
prefix. Parallel publications serialize this check and journal append; a
durable idempotent read is not counted twice. Existing observed evidence is
never silently removed or summarized, and historical context integrity errors
still fail closed.

### On-demand historical observations

A worker can explicitly declare `recall_observation` to retain and retrieve
successful observation tool results within the visible conversation branch.
The runtime uses the existing immutable artifact store and records a
`tool_observation_recorded` publication with its producer, tool call, timestamp,
and artifact digest. No model-generated summary replaces the source bytes.
Capture is bounded to 32 observations per task attempt, 1 MiB of output and
64 KiB of arguments per observation. Oversized, failed, mutating, unknown, and
protocol tool results are not retained. MCP sources require the existing
maintainer-owned `sideEffect: none` and `filesystem: none` policy; gateway
capture occurs after logical authorization and before output truncation.
Dependency artifacts read through `view.artifact_ref` are excluded so their
attempt-local grants do not become historical grants.

The tool supports `action: search` and `action: read`. Search uses literal
model-supplied text in stored arguments and output, returning bounded previews
and opaque observation IDs. It inspects at most 64 authorized candidates or
8 MiB of decoded source data per call. Follow `next_offset` even when a page
has no matching rows; `-1` means the search is exhausted. A read delivers up to
32 KiB of UTF-8 text, with byte offsets and `next_offset` for further fragments.
The runtime validates the original artifact and records a
`tool_observation_read` event before returning content. That event binds the
original publication and digest, original observation time, current read call,
range, content format, and the SHA-256 of the exact delivered fragment.
`inspect trace` exposes the publication and read references without source
content; task transcripts retain the delivered historical evidence.

Access is restricted to the same team and worker identity and the active branch's
visible ancestry. Current source-tool grants, team denials, session denials,
MCP input policy, and network restrictions still apply. An explicit profile
disabling historical memory withholds recall. Bound worksets, coordinators,
and implicit `all` tool grants do not acquire this capability. Reads do not
execute the original tool, become fresh observations, restore prior task cache
entries, or satisfy an objective requirement to perform a new source call.
Storage failure only makes an observation unavailable for reuse; it never
replays a source call. Read integrity or attribution failures return no content.
The current task/run's own observations remain in its normal tool history;
recall excludes that occurrence rather than duplicating its live transcript.

Result schemas can explicitly admit historical evidence with
`x-hufu-tool-evidence.allow_historical: true` and a `historical_pointer` naming
a nullable provenance field in each source object. Their existing extraction
rules produce `evidence_text` and a source target from the immutable result.
Only exact text fragments actually read into the current runner-owned
transcript can support quotations. Runtime assigns the provenance field and
binds the citation to the current recall call while retaining the original
producer reference; search previews and model-supplied provenance earn no
evidence credit. A matching fresh observation takes precedence. Among multiple matches of the
same kind, the first successful matching observation in the current transcript
wins; original source time does not implicitly select a newer observation.
`evidence_text` is the recall response's `content_format` value, not an extension
key or a result-schema property. The response's `content` contains the extracted
fragment. The nullable provenance field's complete schema and pointer semantics
are specified in [Agent format](../reference/agent-format.md#同一對話的歷史觀測結果).
Schemas without
this opt-in retain their current-attempt evidence requirements, including
external-provider submissions without a local transcript.

Teams decide freshness, relevance, and independent verification. For example,
web-research lets each research track reuse its own historical snapshots when
their observation time fits the question; the verifier still opens sources
independently under a fresh-evidence contract. No webpage, URL interpretation,
or team-specific freshness rule lives in the runtime. The first version uses
literal lookup, not semantic ranking, and does not backfill old transcripts.
Changing tool grants or result schemas still requires a new compatible session
under the execution-policy snapshot rules.


### Rejected result candidates

A schema-rejected `submit_result` may produce a `task_result_candidate` event.
The event points to an immutable CAS envelope containing the redacted submission
and its branch, original run/task/attempt, contract, frozen input and accepted
source bindings. The event is appended before the Todo projection is updated;
an append failure publishes neither a candidate nor an accepted result. Task
transition snapshots and replay carry the candidate reference. A new worker
attempt clears it; a result-only continuation retains it.

Repair tools pin one candidate version before provider execution. Changes to the
selected branch, schema, inputs, source results or candidate bytes fail closed.
The runtime applies bounded local edits to a fresh decoded copy, validates the
whole submission, and uses the original occurrence reservation and result commit
path. It rechecks the pinned candidate under the reservation to prevent a stale
repair from winning a race. Rejected intermediate versions are durable; the final
accepted result still depends on the existing result commit and lifecycle gates.
No candidate is used as a dependency result or as acceptance evidence.

`inspect` task JSON exposes candidate version, opaque artifact ID, hash and issue
count, without the raw rejected report. Replay comparison includes candidate
identity. Candidate capture and local repair do not introduce another retry
budget or authorize any execution tools.

A schema-valid local patch is also saved as a new candidate version with
`prepared: true` **before** result submission. This state means only that the
model-facing schema passed, never that evidence or acceptance passed. Recovery
rehydrates its CAS bytes, branch/contract/input/source bindings and original
sealed transcript, checks the static result-only tool policy and the central
execution gate, then runs the original full submission transaction. It requires
no provider access or new model turn. A candidate left in an interrupted worker
state is routed to result finalization, never worker replay. A committed result
whose lifecycle checkpoint was interrupted is finalized using the same result.
An explicit result-only continuation of a blocked, prepared submission uses the
existing operator grant with `additional_attempts: 0` and no replacement model.
It never extends the model budget. The repair receipt records
`prepared_candidate_sha256`; local completion retains
the original run, transcript, repair history and consumed model-turn count.

Submission tool responses carry runtime-authored client metadata distinguishing
`invalid_schema`, `source_invalid`, `contract_rejected`, and `commit_failed`.
Only the first permits another formatting repair. The other reasons stop with
an actionable diagnostic; they do not request another model turn or authorize
worker replay. Legacy responses without this metadata retain their historical
classification. Candidate storage failures are explicit `commit_failed` tool
responses, so the provider adapter cannot discard their classification as an
untyped execution error. External worker proposal protocols are unchanged.
