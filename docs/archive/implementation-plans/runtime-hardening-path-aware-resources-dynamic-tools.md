# Hufu Runtime Hardening Spec
## Path-Aware Resource Claims + Stable Dynamic Tool Surface

> **Audience:** coding agent / maintainer
> **Status:** completed and archived
> **Implemented through:** `33f5059`
> **Repository:** `https://github.com/kjelly/hufu`
> **Baseline branch:** `main`
> **Baseline commit:** `1b97fb789a96f16bbee6c27798007e0f05771fea`
> **Baseline date:** 2026-09-15
> **Design inspiration:** `esengine/DeepSeek-Reasonix`, adapted to Hufu's existing runtime instead of copied literally.
> **Revision:** v1.1 (2026-09-15) — closed three gaps found by an implementation-readiness review: split the former single Stage 3 into a plumbing stage and a separate TOCTOU-safe-I/O stage (§15), named the required scoped-file-access primitive (§6 A5.1), and added file-size/complexity constraints for the already-oversized files this spec touches (§2.2).

---

## 1. Objective

Improve Hufu's multi-agent runtime in two places where the current architecture has enough foundation to gain safety and efficiency without introducing a parallel subsystem:

1. **Path-aware resource scheduling**
   - reuse the existing `ResourceClaim` / DAG scheduler lock model;
   - derive bounded workspace claims from durable `WorksetBinding.TouchedPaths`;
   - enforce those same paths as write confinement before using them to unlock concurrency;
   - keep unbounded/opaque writers conservative.

2. **Stable dynamic tool surface**
   - keep Hufu's existing static tool authorization as the authority;
   - stop exposing every ordinary MCP tool as an independent provider-visible schema;
   - expose a fixed-schema gateway for dynamically discovered/optional MCP tools;
   - preserve direct protocol tools and closed `ToolSequence` semantics.

The purpose is **runtime correctness first**. Prompt-cache stability and lower schema token cost are secondary benefits.

---

# 2. Source-of-truth baseline

Implementation MUST be based on code at or newer than:

```text
kjelly/hufu main
1b97fb789a96f16bbee6c27798007e0f05771fea
```

Before modifying code:

```bash
git rev-parse HEAD
git status --short
```

If HEAD differs, re-read the affected files and preserve any newer semantics. Do not mechanically apply this document against stale code.

Code and tests override older documentation when they disagree.

## 2.1 Existing Hufu components that MUST be reused

Do not create competing abstractions for these:

| Concern | Existing authority |
|---|---|
| durable execution identity | `ExecutionTarget`, `ExecutionTopology`, `ExecutionRegistry` |
| attempt boundary | `AttemptRequest`, `AttemptResult` |
| task execution contract | `ExecutionContract` |
| scheduler resource locking | `ResourceClaim`, `resourceClaims`, `claimsConflict`, `dagScheduler.activeResources` |
| workset path metadata | `WorksetBinding.TouchedPaths`, `NormalizeTouchedPath` |
| external workspace isolation | `ExecutionWorld`, `LocalExecutionWorld` |
| static tool authorization | `ResolveStaticWorkerTools` |
| concrete tool construction | `defaultToolResolver.ResolveTaskTools` |
| worker tool surface | `ResolvedWorkerTools` |
| tool permission | `tools.CheckToolPermissionDetail`, effective tool allowlist |
| MCP authorization | `mcp.ToolAuthorizer`, `mcp.WithToolAuthorizer` |
| repair policy | `RepairController` |
| evidence / acceptance | existing receipt, evidence manifest, verifier, acceptance paths |

Relevant current files:

```text
internal/team/coordinator.go
internal/team/dag_scheduler.go
internal/team/plan_revision.go
internal/team/workset.go
internal/team/invariant_manifest.go
internal/team/execution_world.go
internal/team/execution_world_local.go
internal/team/execution_contract.go
internal/team/task_occurrence_projection.go
internal/team/task_creation_admission.go
internal/team/task_journal.go
internal/team/services.go
internal/team/static_tool_resolution.go
internal/team/subagent_provider.go
internal/team/coordinator_task_run.go
internal/team/coordinator_declared_tool_runner.go
internal/team/tool_policy_gate.go
internal/team/delegation_policy.go
internal/team/model_capability_validation.go
internal/team/coordinator_eventstore.go
internal/team/event_reducers.go
internal/team/cache_policy.go
internal/team/coordinator_taskcache.go
internal/team/execution_receipt.go
internal/team/task_transcript.go
internal/mcp/manager.go
internal/mcp/agent_server.go
internal/tools/tools.go
internal/tools/types.go
internal/tools/view.go
internal/tools/write.go
internal/tools/edit.go
internal/tools/multiedit.go
internal/audit/audit.go
cmd/hufu/display.go
cmd/hufu/json_output.go
cmd/hufu/report.go
docs/architecture/execution-runtime.md
docs/architecture/workset.md
AGENTS.md
```

## 2.2 File-size and complexity constraints

`CLAUDE.md` requires source files to stay under ~800 lines and to be
decomposed when they exceed it. `.golangci.yml` already carries a `gocyclo`
ratchet (`min-complexity: 40`) with per-function exemptions for code that
predates it, annotated in the file itself: "gocyclo debt from lowering
min-complexity 95 -> 40: ... Remove the entry when that function is
decomposed; do not add new ones."

At baseline, three files this spec repeatedly touches are already far over
both budgets:

| File | Current lines | Existing debt |
|---|---|---|
| `internal/team/coordinator_task_run.go` | 5254 | `(*Coordinator).executeTask` is gocyclo-exempted at complexity 95 |
| `internal/team/coordinator.go` | 2939 | defines `ResourceClaim`/`ResourceClaimMode`, which A1 extends |
| `internal/team/coordinator_eventstore.go` | 1673 | — |

These numbers are a snapshot at the baseline commit; re-check with `wc -l`
before each stage, since they only grow across stages.

Rules that apply to every phase/stage below:

1. Do not add new code, branches, or cases to `(*Coordinator).executeTask` or
   any other gocyclo-exempted function. If a stage's behavior must run inside
   that function's control flow (envelope resolution, path-scope
   installation, frozen-tool binding, etc.), extract a narrow, independently
   testable helper into a new or existing sibling file and call it from the
   minimal possible call site.
2. Do not add a new `.golangci.yml` gocyclo (or any other linter) exemption
   entry to land this spec's code. If a change cannot avoid one, stop and
   report it as a spec conflict per section 21 instead of adding the
   exemption silently.
3. New logic this spec introduces that would otherwise land in
   `coordinator_task_run.go`, `coordinator.go`, or `coordinator_eventstore.go`
   — execution-envelope resolution (A4), resource-scope snapshot
   binding (A4.1), dynamic-tool snapshot binding (B4.2), path-scope
   installation (A5) — MUST be implemented in new sibling files (for example
   `coordinator_task_execution_envelope.go`, `coordinator_resource_scope.go`,
   `coordinator_dynamic_tool_authorization.go`) and wired in with the smallest
   possible call-site diff in the existing file. Do not grow these three
   files' line counts beyond what that call-site wiring requires.
4. This does not require decomposing these three files down to under 800
   lines as a prerequisite — that is a larger, separate refactor and is out
   of scope for this spec. It only requires that this spec's new work not
   make the existing size/complexity debt worse.
5. If a stage genuinely cannot be implemented without materially growing one
   of these three files beyond call-site wiring, stop and flag it explicitly
   in that stage's PR description rather than silently exceeding the
   guideline further.

---

# 3. Current-state findings

## 3.1 Resource scheduling already exists

Current Hufu already has:

```go
type ResourceClaimMode string

const (
    ResourceRead      ResourceClaimMode = "read"
    ResourceWrite     ResourceClaimMode = "write"
    ResourceExclusive ResourceClaimMode = "exclusive"
)

type ResourceClaim struct {
    Resource string
    Mode     ResourceClaimMode
}
```

The DAG scheduler already tracks active claims and delays conflicting tasks.

Current conflict semantics are **exact resource-string equality**:

```text
resource "repo" == resource "repo"    -> conflict depending on mode
resource "repo/a" != resource "repo"  -> no hierarchical relationship
```

This is insufficient for repository paths.

## 3.2 Worksets already contain bounded path information

`WorksetItem` / `WorksetBinding` already carry:

```go
TouchedPaths []string
```

and paths are normalized through the repository-relative `NormalizeTouchedPath`.

This MUST remain metadata associated with a workset. **Path MUST NOT become workset identity.**

## 3.3 LocalExecutionWorld already has a whole-workspace lease

`LocalExecutionWorld.Prepare()` currently acquires a process-local lease by canonical workspace root before taking the baseline snapshot.

This exists for a correctness reason:

```text
baseline snapshot
   ↓
external/native provider mutation
   ↓
post snapshot
   ↓
WorkspaceDelta
```

If two such attempts mutate the same root concurrently, each delta can accidentally contain the other's changes.

Therefore this spec MUST NOT replace that lease with path-level concurrency until Hufu has per-attempt mutation attribution that makes whole-root snapshot deltas safe.

## 3.4 Tool resolution currently expands MCP schemas

Current `defaultToolResolver.ResolveTaskTools()`:

```text
base tools
+ MCPToolManager.AsAgentTools()
+ agent-specific MCP tools
        ↓
ResolveStaticWorkerTools()
        ↓
concrete fantasy.AgentTool list
        ↓
model-visible schemas
```

As MCP inventory grows, the model-visible schema surface grows and changes.

The current `ResolvedWorkerTools` intentionally conflates:

```text
provider-visible tool surface
runtime authorization names
```

A stable dynamic gateway requires separating those two concepts without weakening authorization.

## 3.5 Current mutation normalization prevents path-level concurrency

`coordinator_execute.go` calls `serializeMutationTasks` before effective
resource claims exist. That helper adds a dependency from every mutation task
to the previous mutation task, so changing only `dagScheduler.resourceConflict`
cannot make disjoint writers overlap. Phase A must replace this early blanket
normalization with the post-envelope conflict-aware normalization in A6.

## 3.6 Current path and MCP metadata are insufficient for the target contracts

`mergedAllowedWritePaths` currently unions configured and context-provided
write paths. `WorksetBinding.TouchedPaths` is not installed as an attempt read
scope, and structured file tools validate a path before reopening it by name
for I/O. Those facts prevent narrow claims from serving as an enforced
concurrency boundary without the A5 changes.

At MCP discovery, `MCPTool` retains Fantasy's `Parameters` and `Required`
projection but not the complete input schema. That is sufficient for direct
provider exposure but not for host-side gateway validation or a durable
descriptor fingerprint; section 11 therefore adds a deep-copied full schema
without changing the direct projection.

---

# 4. Non-negotiable invariants

These invariants are more important than feature completion.

## INV-01 — Authorization and scheduling are different

A `ResourceClaim` is a scheduling/conflict declaration.

It MUST NOT grant filesystem authorization.

A task that claims:

```text
workspace:path:internal/team
```

does not gain permission to write that path unless existing Hufu path/tool policy already permits it.

## INV-02 — Narrow concurrency requires enforced workspace scope

Hufu may use a path claim to permit concurrent writers only when the runtime can enforce that the task cannot write outside that claim.

It must also confine attempt-time workspace reads to the claimed read set;
otherwise a nominally disjoint worker can observe another writer's concurrent
mutation and invalidate deterministic scheduling.

If the read/write scope cannot be enforced, the task MUST fall back to the
appropriate whole-workspace read or exclusive claim.

The scheduling claims and enforced read/write scopes MUST come from one immutable
preflight result. The scheduler MUST NOT independently re-derive a narrower
claim than the executor installs.

## INV-03 — External provider snapshot isolation remains whole-root

`LocalExecutionWorld` whole-root lease stays in force for native external-provider attempts using whole-tree before/after snapshots.

Do not trade delta correctness for scheduler concurrency.

## INV-04 — Workset identity is unchanged

`TouchedPaths` are scheduling/authorization inputs only.

Do not include path text as the identity of a workset item.

Existing source artifact + digest + item key / expansion receipt semantics remain canonical.

## INV-05 — Durable occurrence owns execution inputs

Any path claims that affect execution MUST be reproducible from the frozen Todo/task occurrence.

Do not accept a new mutable claim from `AttemptRequest` or an external provider response.

Retry/resume must resolve the same effective path claims from durable state.

The same rule applies to manager-owned MCP authorization covered by Phase B.
A resumed or retried occurrence may lose an unavailable manager target, but it
MUST NOT gain one that was absent from its frozen dynamic-tool snapshot.

## INV-06 — Static tool resolution remains authorization authority

`ResolveStaticWorkerTools` continues deciding which logical tools are authorized.

The dynamic gateway only changes **representation / dispatch**, not authorization.

## INV-07 — Gateway discovery cannot widen capability

`search` and `inspect` MUST return only targets already authorized for the current attempt.

A model cannot discover a hidden target and thereby make it callable.

## INV-08 — Gateway target authorization is checked at call time

Calling the gateway is not sufficient authorization.

Before dispatch, the exact logical target MUST pass the same current Hufu policy/MCP authorizer checks as direct invocation.

## INV-09 — Protocol tools stay direct

At minimum, these remain direct provider-visible tools:

```text
submit_result
submit_plan
```

Result-repair/resume mode MUST retain the existing exact surface invariant.

## INV-10 — Closed ToolSequence stays semantically exact

A target explicitly present in `Execution.ToolSequence` MUST remain directly addressable in v1.

Do not silently replace:

```text
server__deploy
```

with:

```text
use_dynamic_tool(call target=server__deploy)
```

until ToolSequence itself has a separately reviewed logical-target protocol.

## INV-11 — No completion by prose

Do not change Hufu's existing completion rule.

Success still requires the applicable typed result, objective verification, receipts, evidence and acceptance gates.

## INV-12 — Fail closed on ambiguity

Unknown resource namespaces, malformed workspace path resources, missing gateway target mappings, stale target IDs, or authorization disagreement MUST fail closed.

For compatibility, arbitrary authored generic resource strings are not treated
as namespaces. "Unknown resource namespace" here means a string that starts
with the reserved `workspace:` namespace but is not a valid
`workspace:path:<path>` resource.

## INV-13 — Preflight is atomic with respect to dispatch

All task execution envelopes for an initial scheduler batch MUST be resolved
before the scheduler launches the first task. A static error in claim
normalization, workspace-scope derivation, frozen tool authorization, gateway
projection, or cache identity MUST reject the batch before any worker or
side-effect process starts.

Retries and resume re-resolve an envelope from the durable occurrence and its
frozen snapshots. Failure to reproduce it blocks that occurrence; it never
causes a broader fallback authorization.

---

# 5. Scope

## In scope

### P0-A
Hierarchical workspace resource claims and workset-derived write isolation.

### P0-B
Stable dynamic MCP tool gateway with fixed provider-visible schema.

### P0-C
Observability and regression tests proving the changes do not weaken:
- task occurrence durability;
- tool authorization;
- workset semantics;
- execution-world isolation;
- result-only / plan lifecycle tool surfaces;
- task-cache freshness.

## Explicitly out of scope

Do NOT implement in this change set:

- a new `ExecutionContract` type;
- a second resource-lock manager;
- replacing `LocalExecutionWorld` whole-root lease;
- file-snapshot rewind / Claude-Code-style `/rewind`;
- a new pause-policy subsystem;
- Extension Protocol / plugin sidecars;
- lazy MCP process startup;
- arbitrary built-in tools behind the gateway;
- Skill execution behind the gateway;
- changing workset identity;
- changing acceptance/evidence authority;
- changing `RepairController` policy.

Checkpoint/rewind can be a future spec after this work establishes reliable mutation ownership.

---

# 6. Phase A — Hierarchical workspace resource claims

## A1. Preserve the existing ResourceClaim data model

Do not add another top-level claim struct.

Keep:

```go
type ResourceClaim struct {
    Resource string
    Mode     ResourceClaimMode
}
```

Introduce a reserved resource namespace:

```text
workspace:path:<normalized-repository-relative-path>
```

Examples:

```text
workspace:path:.
workspace:path:internal/team
workspace:path:internal/team/services.go
workspace:path:docs
```

Non-prefixed existing resources retain exact-match semantics:

```text
repo
vm
database:production
deployment:staging
```

`workspace:path:` is relative to the envelope's canonical mutable project or
workflow root. It does not name Hufu's coordinator-owned control workspace
(`session.json`, logs, receipts, event stores, memory, or `shared/` handoff
files). `TaskDef.ContextFiles` already names validated files beneath that
control workspace and is not converted into a repository claim. If physical
roots overlap, task write-scope resolution must reject coordinator-owned
control paths; it must not authorize them through a broad touched path.

## A2. Add canonical helpers

Required v1 API shape:

```go
const workspacePathResourcePrefix = "workspace:path:"

func NewWorkspacePathResourceClaim(path string, mode ResourceClaimMode) (ResourceClaim, error)

func ParseWorkspacePathResource(resource string) (path string, ok bool, err error)

func normalizeResourceClaim(claim ResourceClaim) (ResourceClaim, error)

func normalizeResourceClaims(claims []ResourceClaim) ([]ResourceClaim, error)

func resourceClaimConflicts(a, b ResourceClaim) bool
```

Rules:

1. `"."` is a reserved resource-only value meaning the whole
   repository/workspace namespace. Handle it before calling
   `NormalizeTouchedPath`, which intentionally rejects dot segments for
   workset metadata.
2. Normalize every other workspace path through the same repository-relative
   contract used by `NormalizeTouchedPath`, then remove one trailing `/` from
   the resource representation. Workset metadata may retain its directory
   marker; scheduler identity does not. Thus `docs`, `docs/`, and the resource
   form derived from either all become `workspace:path:docs`.
3. Reject:
   - absolute paths;
   - parent escapes;
   - empty path after the prefix;
   - dot segments other than the reserved whole-root `.`;
   - backslashes;
   - malformed or unknown reserved `workspace:` namespaces;
   - an unknown `ResourceClaimMode`.
4. Store canonical slash-separated path text and canonicalize an empty mode to
   `exclusive` before hashing, comparison, or persistence of resolved
   diagnostics.
5. Do not resolve symlinks here; this is scheduling identity, not filesystem authorization.
6. `ParseWorkspacePathResource` returns `(path, false, nil)` only for ordinary
   generic resources that do not start with `workspace:`. Any malformed
   reserved `workspace:` value returns an error.
7. Preserve ordinary generic resource text byte-for-byte and its current exact
   equality semantics. For compatibility, `normalizeResourceClaims` drops an
   empty generic resource just as current conflict checks do.
8. Coalesce duplicate canonical resources to their strongest mode using
   `exclusive > write > read`, then sort by resource and mode for persisted
   diagnostics, claim digests, and scheduler envelopes.
9. The slice helper returns a detached slice and never mutates authored task
   data.

## A3. Hierarchical conflict semantics

For ordinary non-workspace resources, preserve current exact equality.

For `workspace:path:` resources, two claims overlap if:

```text
same path
OR A is ancestor of B
OR B is ancestor of A
OR either is "."
```

Ancestor matching is segment-boundary matching: `docs` is an ancestor of
`docs/a.md`, but not of `docs2/a.md`.

Mode compatibility remains:

```text
read + read         -> compatible
read + write        -> conflict
write + write       -> conflict
exclusive + any     -> conflict
```

Examples:

| A | B | Result |
|---|---|---|
| `workspace:path:docs` read | `workspace:path:docs` read | compatible |
| `workspace:path:docs` read | `workspace:path:docs/a.md` write | conflict |
| `workspace:path:internal/team` write | `workspace:path:internal/team/foo.go` write | conflict |
| `workspace:path:internal/team` write | `workspace:path:docs` write | compatible |
| `workspace:path:.` write | `workspace:path:docs` read | conflict |
| `repo` write | `workspace:path:repo` write | no special relation; different namespace |

Add explicit regression cases for `docs/` versus `docs/a.md`, duplicate
equivalent path spellings, an invalid mode, `workspace:other:x`, and the
resource-only `.` special case.

## A4. Separate authored claims from effective runtime claims

Do not overload the existing static validation function with hidden runtime derivation.

Required split:

```go
func authoredResourceClaims(task TaskDef) []ResourceClaim

type EffectiveTaskResourceScope struct {
    Claims            []ResourceClaim
    AllowedReadPaths  []string // canonical absolute attempt-local paths
    AllowedWritePaths []string // canonical absolute attempt-local paths
    BoundedReadScope  bool
    BoundedWriteScope bool
    Source             string
}

type TaskExecutionEnvelope struct {
    OccurrenceDigest string
    Tools            ResolvedWorkerTools
    ResourceScope    EffectiveTaskResourceScope
    LogicalToolsetDigest string
    ResourceScopeDigest  string
}

func (c *Coordinator) resolveTaskExecutionEnvelope(
    ctx context.Context,
    task TaskDef,
    todo *TodoItem,
) (TaskExecutionEnvelope, error)
```

`TaskExecutionEnvelope` is the single attempt-preflight product shared by the
scheduler, task cache, and executor. Use these names in v1 so the scheduler,
cache, and executor cannot grow competing representations.

The envelope is attempt-local and is rebuilt on retry/resume. Its occurrence
digest, resource scope, names, catalogs, descriptors, and digests are detached
and immutable after admission. Concrete Fantasy handlers remain attempt-owned
objects and may receive their existing provider options; that permitted
handler mutation must not alter any frozen envelope metadata.

The resolver MUST:

1. reconstruct the canonical task from either the not-yet-persisted prospective
   `TodoItem` used by task-creation admission or the durable Todo occurrence
   used by retry/resume;
2. verify that the caller-supplied task matches that prospective/durable
   occurrence;
3. resolve the frozen logical tool authorization described in Phase B;
4. determine the canonical execution backend and mutable work root;
5. derive and validate read/write-scope enforcement capability;
6. calculate effective claims and cache digests;
7. return without calling a model or starting a worker/action process.

For a new candidate, the batch-admission caller first attaches the dynamic-tool
snapshot, binds tools, attaches the resource-scope snapshot, and then calls the
envelope resolver for a final cross-check. For retry/resume, both snapshots
must already come from the durable Todo; the envelope resolver only validates
and binds them.

For the initial batch, resolve every envelope before `dagScheduler.run` calls
`launchReady`. Store the envelopes by batch index. Do not recompute claims in
`resourceConflict` or when inserting into `activeResources`.

`RunDirectAgent` and other single-task creation paths use the same sequence as
a batch of one; they do not bypass either occurrence snapshot because no DAG
scheduler is present.

Compatibility wrappers may retain the current `resourceClaims` name if
necessary, but authored-only and effective-runtime call sites must be explicit.

### A4.1 Durable resource-scope snapshot

`WorksetBinding` alone is insufficient to reproduce an effective scope because
eligibility also depends on the authorized tool/backend surface. Freeze the
relative scheduling/confinement result in the occurrence:

```go
const taskResourceScopeSnapshotVersion = 1

type TaskResourceScopeSnapshot struct {
    Version           int             `json:"version"`
    Claims            []ResourceClaim `json:"claims"`
    ReadPaths         []string        `json:"read_paths,omitempty"`
    WritePaths        []string        `json:"write_paths,omitempty"`
    BoundedReadScope  bool            `json:"bounded_read_scope,omitempty"`
    BoundedWriteScope bool            `json:"bounded_write_scope,omitempty"`
    Source            string          `json:"source"`
    Digest            string          `json:"digest"`
}
```

Add this exact field to `TodoItem`, `TodoSpec`, and
`TaskOccurrenceProjection`:

```go
ResourceScopeSnapshot *TaskResourceScopeSnapshot `json:"resource_scope_snapshot,omitempty"`
```

Do not expose this as an authorable JSON/YAML `TaskDef` field. Update
`todoItemFromSpec`, `newTaskOccurrenceProjection`, projection clone/equality,
and `compareTaskDefWithTodoOccurrence` so the caller-supplied TaskDef is
compared for authored fields while both runtime-owned snapshots are copied
only from the prospective/durable Todo. A model-provided payload can never
choose its own frozen claims or digests.

`Claims` contains canonical resource identities; `ReadPaths` and `WritePaths`
contain only canonical repository-relative paths. Never persist absolute
attempt roots. Sort and de-duplicate them, deep-clone at every projection
boundary, and compute
`Digest` with the section 12 length-prefixed encoder over version, flags,
source, claims, and paths. Include the snapshot in the occurrence/decision
digest and every creation, transition, session, event-replay, journal, branch,
and shadow projection that carries the immutable task contract.

Scope paths preserve `NormalizeTouchedPath`'s trailing `/` directory marker;
workspace resource claims use A2's marker-free canonical identity. Scope
containment must honor the marker and path-segment boundaries rather than
consulting mutable filesystem type during hashing.

Hash records in this exact order:

```text
resource_scope_version, "1"
source, <source>
bounded_read_scope, <true|false>
bounded_write_scope, <true|false>
claim, <resource>, <mode>       (resource/mode-sorted)
read_path, <relative path>      (path-sorted)
write_path, <relative path>     (path-sorted)
```

On admission/restore, require version 1, the closed `source` enum from A9,
sorted unique canonical slices, a recomputed digest match, and consistency
between flags, paths, claims, and effective side effect. In particular,
`BoundedWriteScope` implies `BoundedReadScope`, bounded flags require non-empty
matching paths, every bounded write path is covered by `ReadPaths` and has a
corresponding write claim, and an
unbounded new occurrence has the required whole-root read/exclusive claim. A
non-nil invalid snapshot is `resource_snapshot_invalid`; never reinterpret it
as legacy.

Required resolver split:

```go
func (c *Coordinator) resolveNewTaskResourceScope(
    task TaskDef,
    todo *TodoItem,
    tools ResolvedWorkerTools,
) (*TaskResourceScopeSnapshot, error)

func (c *Coordinator) bindFrozenTaskResourceScope(
    task TaskDef,
    todo *TodoItem,
    tools ResolvedWorkerTools,
    mutableRoot string,
) (EffectiveTaskResourceScope, error)
```

For new work, compute the snapshot after frozen tool authorization and concrete
tool-surface projection, attach it to the prospective Todo, then run occurrence
admission. On retry/resume, do not re-derive narrower claims from the current
tool list. Validate the snapshot digest, map its relative paths under the
canonical current mutable root, and prove the current surface can still enforce
every frozen bounded access. Lost capability, a newly exposed unsupported
workspace-access tool, root drift, or an empty configured-scope intersection
blocks preflight; it never widens or silently narrows the frozen claims.

A nil snapshot identifies a legacy occurrence. Bind it conservatively as
`workspace:path:. read` for `SideEffectNone` and
`workspace:path:. exclusive` for every writing/unknown side-effect class; do
not grant bounded concurrency to legacy work. Preserve and normalize its
authored generic/resource claims in addition to the whole-root compatibility
claim.

### Authored claims

Include current:

```text
TaskDef.Resources
legacy TaskDef.ResourceClaims -> exclusive
```

Static team/plan lint keeps evaluating authored claims as it does today.
`validateResourceClaims` must normalize and validate every authored claim
before pair comparison. The boolean `claimsConflict` compatibility helper may
assume canonical input only at prevalidated call sites; if retained for raw
legacy callers, invalid input must conservatively report a conflict and the
admission path must return the original normalization error. Never turn an
invalid claim into "no conflict" because a boolean helper cannot return an
error.

Preserve the current semantic distinction: conflicting authored claims in a
declared parallel plan are rejected by static lint, while conflicts introduced
only by effective workset-derived runtime claims are serialized by the DAG
scheduler.

### Effective runtime claims

Start from authored claims, then add Hufu-owned workspace claims.

Derivation:

#### SideEffectNone

If `WorksetBinding.TouchedPaths` is non-empty and every authorized
workspace-reading tool can enforce the attempt read scope:

```text
workspace:path:<touched> mode=read
```

The configured read allowance must contain every touched path. A configured
descendant does not silently narrow a touched ancestor; that is an admission
error because the frozen work item could no longer be completed as declared.

If any authorized reader cannot enforce the touched-path scope, use the
following conservative rule for v1:

```text
workspace:path:. mode=read
```

V1 has no claim-elision exception for a "workspace-free" backend or action.
This ensures a whole-workspace writer actually conflicts with every unbounded
reader and avoids introducing a second read-capability taxonomy.

#### SideEffectWorkspaceWrite

If the execution envelope proves that Hufu can install enforced task write
paths from the durable workset binding and can confine all attempt-time reads
to the touched-path read set:

```text
workspace:path:<touched> mode=write
```

The configured/runtime write allowance must contain every touched path. A
configured descendant does not silently narrow a touched ancestor; that is an
admission error because the frozen work item could no longer be completed as
declared.

If it cannot enforce the bounded write paths, or no touched paths are available:

```text
workspace:path:. mode=exclusive
```

#### SideEffectExternalWrite / SideEffectInfraMutation

Do not infer that external systems are represented by repository paths.

For workspace mutation safety, use:

```text
workspace:path:. mode=exclusive
```

unless a narrower Hufu-owned execution boundary is objectively enforced.

Existing authored resource claims still describe external resources separately.

#### SideEffectCredential

Do not use a path claim to make credential mutation safe. Existing
execution-world admission rules remain authoritative. If such a task can also
touch the workspace, add `workspace:path:. exclusive`; this is only scheduler
conservatism and never grants credential authority.

## A5. Read/write confinement requirement

Before a `SideEffectWorkspaceWrite` task receives a narrow path-level write
claim, the execution envelope must prove that the same bounded paths will be
installed into Hufu's write-isolation mechanism and that its touched-path read
set will be installed into the read boundary. A
`SideEffectNone` task receives narrow read claims only when that read boundary
is enforceable.

Use the existing context boundary:

```text
tools.AgentAllowedWritePathsKey
```

Treat `AgentAllowedPathsKey` and `AgentAllowedWritePathsKey` as inputs while
building the envelope; stop re-installing raw `runtimeAllowedWritePaths()` as
the final task scope. Add one explicit attempt context value:

```go
type AgentTaskPathScope struct {
    ReadPaths    []string
    WritePaths   []string
    ReadBounded  bool
    WriteBounded bool
    Digest       string
}

var AgentTaskPathScopeKey agentTaskPathScopeKeyType
```

Install a detached copy from the admitted envelope on every worker/tool
execution context. `cfgWithMergedPaths` first computes the existing effective
configured/runtime allowances, then applies this task scope as the final
intersection. Do not overload an old key with both union and intersection
meanings.

Change its merge semantics for a present, non-empty task scope from union to
intersection. The required pure helper behavior is:

```go
func IntersectWritePathScopes(configured, runtime []string) ([]string, error)
func IntersectReadPathScopes(configured, task []string) ([]string, error)
func IntersectPathScopeCeilings(requested []string, ceilings ...[]string) ([]string, error)
```

Treat `configured` as an authorization ceiling and `runtime` / `task` as the
requested complete scope. Each requested path must be equal to or below at
least one configured path; retain the requested path. A configured descendant
does not count as containment and disjoint/partial coverage is an admission
error. Canonicalize and de-duplicate the result. Semantics are:

```text
runtime == nil           -> preserve current configured behavior
runtime scope present,
configured len == 0      -> runtime scope
both present             -> retain every fully contained requested path
any requested path not fully contained -> admission error
```

A non-nil runtime/task slice is the explicit attempt scope. A non-nil empty
slice is invalid when bounded access was requested; it does not mean
"unrestricted". The helper returns `nil` only when the runtime scope is absent
and the current configured behavior is also unbounded. The context installer
and enforced tools inspect `ReadBounded` / `WriteBounded`, not only slice
length. A bounded flag with an empty path list or mismatched digest fails
closed before I/O.

The two named helpers are compatibility wrappers. Envelope resolution uses
`IntersectPathScopeCeilings`: for each requested path, every non-empty ceiling
group must contain it under at least one path in that group. The effective
configured/runtime read allowance is one read ceiling group; agent configured
write paths and `runtimeAllowedWritePaths()` are separate write ceiling groups.
This avoids incorrectly intersecting two broad ceilings before the concrete
task path is known.

Configured/runtime allowance entries are authorization roots and therefore
contain descendants. A requested scope path without trailing `/` names only
that exact path; a requested path with `/` names its subtree. All comparisons
use canonical absolute forms at admission and segment boundaries, never raw
string prefix matching.

Rules:

1. source paths from the **durable** `WorksetBinding.TouchedPaths`;
2. normalize them again defensively;
3. resolve them under the same canonical mutable work root passed to the
   worker tools; do not assume that `session.Workspace`, `projectDir`, the
   workflow runtime root, and an `ExecutionWorld.Root` are interchangeable;
4. intersect with any already-active runtime write restriction;
5. never widen `runtimeAllowedWritePaths()`;
6. when the intersection is empty for a write task, fail admission rather than silently widening;
7. resolve touched paths into the explicit read scope, then intersect it with
   the current effective configured/runtime read allowance;
8. an empty read intersection is an admission error; never widen past the
   configured read allowance. A tool/backend that cannot enforce the otherwise
   valid task scope uses a whole-root scheduling claim while retaining its
   current authorization boundary;
9. an attempted read or write outside its bounded set must fail before the
   filesystem operation.
10. install the scope consistently in `coordinator_run.go`,
    `coordinator_task_run.go`, `coordinator_declared_tool_runner.go`, and any
    hufu-local backend path that executes the same Todo. Missing installation
    for a bounded snapshot is `resource_scope_unreproducible`, not an
    unbounded fallback.

### A5.1 Narrow-scope eligibility

Add this v1 tool behavior taxonomy in `internal/tools`:

```go
type PathScopeBehavior string

const (
    PathScopeEnforced    PathScopeBehavior = "enforced"
    PathScopeDenied      PathScopeBehavior = "denied_when_scoped"
    PathScopeUnsupported PathScopeBehavior = "unsupported"
)

type ToolWorkspaceScopeDescriptor struct {
    MayReadWorkspace  bool
    ReadBehavior      PathScopeBehavior
    MayWriteWorkspace bool
    WriteBehavior     PathScopeBehavior
}

type ToolWorkspaceScopeDescriber interface {
    DescribeWorkspaceScope() ToolWorkspaceScopeDescriptor
}

func DescribeToolWorkspaceScope(fantasy.AgentTool) ToolWorkspaceScopeDescriptor
```

Known non-filesystem observation/protocol tools return both `May*` fields
false. Unknown, custom, MCP, gateway-call, terminal/native-process, bash, Lua,
sudo, ssh, download, grep, glob, ls, and external tools conservatively return
the access they may perform plus `PathScopeUnsupported`. `enforced` means the
handler checks the canonical attempt scope immediately before every relevant
filesystem operation. `denied_when_scoped` means a non-empty task scope makes
the handler fail before any process or filesystem operation. Do not infer
behavior from a tool name at the scheduler call site.

Extend the private `internal/tools.coreTool` with an explicit descriptor set by
each constructor. `DescribeToolWorkspaceScope` may read that field or the
`ToolWorkspaceScopeDescriber` interface; it must not bless an arbitrary custom
tool merely because `Info().Name` is `view` or `write`. Constructors that omit
the descriptor receive the conservative unknown default.

For v1, only `view`, `write`, `edit`, and `multiedit` may become
`PathScopeEnforced`. Implement the shared scoped file-access helper on top of
Go's directory-relative root API — `os.OpenRoot` and `*os.Root`'s `Open`,
`OpenFile`, `Create`, `Lstat`, `Mkdir`/`MkdirAll`, and `Rename` methods
(stable since Go 1.24; this module already targets `go 1.26.5` per `go.mod`)
— rather than hand-rolling `openat`/`unlinkat` syscalls. Open the canonical
mutable root once via `os.OpenRoot`; every subsequent parent walk, create,
read, or rename goes through that `*os.Root` handle so no step re-resolves a
plain absolute path, and no path component can name a location outside the
root on any platform `os.Root` supports. Per the standard library's own
documentation, `os.Root` does not guarantee root confinement on `GOOS=js`
(explicitly documented as "vulnerable to TOCTOU... and cannot ensure that
operations will not escape the root") and behaves differently on
`GOOS=plan9`; treat those targets, and any other platform where `os.Root`'s
documented guarantees do not hold, as lacking this primitive.

`os.Root` methods still *follow* an in-root symbolic link by design ("Methods
on Root will follow symbolic links, but symbolic links may not reference a
location outside the root"), so root confinement alone does not satisfy "do
not follow the final symlink" for reads: before every enforced read,
`Root.Lstat` the leaf name first and fail closed if it reports a symlink
instead of a regular file, then `Root.Open` it. Writes/edits create a new
same-directory temporary regular file via `Root.OpenFile`/`Root.Create` and
replace the final path with `Root.Rename`; `Root.Rename` replaces whichever
directory entry currently holds that name — regular file, symlink, or hard
link — without following it, which is exactly the required "replaced rather
than followed" semantics, so no separate unlink-then-link step is needed.
Edit/multiedit compare the original file identity (`Root.Lstat` plus
`os.SameFile`) immediately before rename and fail on concurrent replacement.
Authorization and I/O must not be separated by an unvalidated absolute path
lookup. Mark each tool enforced only after all of its read/create/overwrite/
edit branches use the helper and the symlink-race tests (A-T14) pass. A
platform without a working `os.Root` confinement guarantee reports them as
`unsupported` and uses whole-root fallback.

Before rename, require the destination to be absent or a regular file under
the still-open validated parent; reject directories and special files. Clean
up temporary files on cancellation/error. Preserve existing mode bits on
replacement and use the tool's current creation mode for new files.

Existing bash `WorkflowBoundedBash` command matching is not an OS write
boundary and does not qualify bash as enforced or denied.

Compute descriptors from the concrete candidate handlers before
`policyGatedTool` or provider-compatibility wrapping and carry the immutable
result in `ResolvedWorkerTools.WorkspaceScopeDescriptors`, keyed by the already
collision-checked logical name. A wrapper may preserve but never upgrade its
inner descriptor. Hufu-owned protocol and observation tools that live outside
`internal/tools` implement `ToolWorkspaceScopeDescriber`; missing map entries use
the unknown/unsupported default.

A task is eligible for narrow writer claims only when all of these are true:

- it has non-empty durable `TouchedPaths`;
- its canonical mutable root is known and maps those repository-relative paths
  without changing their meaning;
- its backend executes through Hufu's gated local tool boundary;
- every authorized workspace-reading tool is read-enforced or denied before
  I/O under the explicit read scope;
- every authorized mutation-capable tool is `PathScopeEnforced` or
  `PathScopeDenied`;
- every workspace access performed by a tool required by a closed
  `ToolSequence` is `PathScopeEnforced` (a required tool that would be denied
  under the scope is an admission error);
- no external/native agent backend can mutate the tree outside those handlers.

In v1, `execution.BackendKindAgent` is never eligible for narrow writer
claims. `LocalExecutionWorld` post-run delta validation detects an escape but
does not prevent the mutation, and the Codex workspace-write sandbox currently
receives the whole root. Such tasks use `workspace:path:. exclusive`.

If a local LLM workspace-writing task exposes any unsupported workspace read
or mutation path, fall back to `workspace:path:. exclusive`; do not remove the
tool merely to obtain more concurrency. A read-only task with an unsupported
reader falls back to `workspace:path:. read`. Existing independent policy may
still deny that tool.

Directory/file authorization must use canonical-path checks immediately before
I/O. Add a concurrency regression in which one task attempts to replace a
validated parent with a symlink while another writes. If the existing
path-then-write implementation cannot close that TOCTOU window, bounded
concurrent writers remain disabled for the affected tool until a descriptor-
relative/openat-style implementation or an equivalent pre-mutation boundary is
available.

Do not trust free-form task prose as a path claim.

Do not derive read or write authorization from `TaskDef.Resources`.

`ResourceClaim` remains scheduling-only.

## A6. Scheduler integration

Change the DAG scheduler's active claim path from authored-only claims to the
precomputed `TaskExecutionEnvelope.ResourceScope.Claims`.

The batch-admission resolver resolves every envelope. `newDAGScheduler`
accepts the complete precomputed slice and returns an error for a missing,
duplicate, index/task mismatch, or invalid digest. `launchReady` remains free
of fallible claim derivation. This prevents a later malformed task from being
discovered only after an earlier task has already started.

Store effective claims for the active task:

```go
activeResources map[int][]ResourceClaim
```

No second lease table.

Cache lookup and in-flight de-duplication also consume the envelope's logical
toolset digest and resource-scope snapshot digest. An envelope mismatch or
missing envelope is a fail-closed scheduler error, not an empty-claim fallback.

The current early `serializeMutationTasks(tasks)` call would add a dependency
between every mutation before effective claims exist and would therefore make
disjoint writer concurrency impossible. Remove that early call. After all
candidate envelopes are resolved but before occurrence admission, run:

```go
func serializeConflictingMutationTasks(
    tasks []TaskDef,
    envelopes []TaskExecutionEnvelope,
) []TaskDef
```

Preserve authored dependencies. For each mutation task, add dependencies on
every earlier mutation task whose effective claims conflict, unless the edge is
already present. Do not add an implicit edge between disjoint bounded writers.
Because implicit edges point only to lower batch indexes, this normalization
cannot itself introduce a forward edge; run the existing cycle/dependency
validation, workflow validation, and delegation-policy validation again on the
final tasks. Rebuild the prospective Todo projections and final envelope
occurrence cross-check after adding the edges so `task_created` persists
exactly the DAG that will execute.

This preserves durable ordering/failure semantics for potentially overlapping
mutations while allowing the scheduler to overlap genuinely disjoint bounded
writers. Read/write conflicts are scheduler leases, not new failure-dependency
edges, because the existing normalization only orders mutation tasks.

### A6.1 Preflight failure semantics

Preflight is an admission boundary, not a worker failure:

- reserve IDs and construct prospective `TodoItem` values in memory, resolve
  all snapshots and envelopes, then pass those exact projections through the
  existing `validateTaskCreationAdmission` / `CommitTaskCreationResolved`
  boundary. If any initial-batch item fails, reject the whole delegation before
  `task_created`, create no Todos, acquire no leases, perform no cache lookup,
  and start no worker;
- if retry/resume cannot reproduce an existing occurrence envelope, do not
  transition it to `in_progress`. Use the existing canonical failure event and
  transition machinery to persist `FailurePolicy`, `NeedsHuman`, and
  `TaskBlocked` with a stable preflight reason code;
- configuration, authorization, descriptor, or scope mismatches are
  non-retryable until the durable occurrence or environment changes. Do not
  spend an LLM retry on them;
- transient provider/model failures occur after admission and retain their
  current retry semantics.

Required preflight reason codes are:

```text
invalid_resource_claim
invalid_write_scope
resource_snapshot_invalid
resource_scope_unreproducible
mutable_root_drift
unsupported_closed_sequence_scope
tool_name_collision
dynamic_snapshot_invalid
dynamic_target_missing
dynamic_descriptor_changed
tool_surface_mismatch
```

The canonical `FailureEventPayload` contains only task/occurrence identity,
the bounded reason code/summary, and bounded target/resource identity. It
contains no tool arguments, schemas, paths outside the canonical
workspace-relative form, credentials, or provider payloads. Do not add a
second preflight-failure event schema.

### A6.2 New-batch admission order

Use this exact side-effect-free-before-commit order for `ExecuteTasks` and the
batch-of-one direct path:

1. run existing task, agent, workflow, delegation, resource, and cycle
   validation without `serializeMutationTasks`;
2. reserve Todo IDs and build prospective Todo projections in memory;
3. resolve all dynamic authorization snapshots and bind candidate tools;
4. resolve all resource-scope snapshots and preliminary envelopes;
5. add only conflicting-mutation implicit dependencies, then rerun workflow,
   delegation, dependency, and cycle validation;
6. rebuild prospective Todos with the final edges, retain the already computed
   immutable snapshots, and resolve/final-check every envelope again;
7. create decision-admission markers from those exact occurrence projections;
8. call `CommitTaskCreationResolved`; on failure discard all in-memory
   envelopes not associated with a committed occurrence;
9. emit `resource_claims_resolved`, then allow cache lookup, lease acquisition,
   and scheduler/worker dispatch.

Steps 1–7 may read already-loaded descriptors, filesystem metadata, and
read-only model capability metadata, but they must not call a language model,
invoke a tool/action, start a new MCP server, acquire an execution lease, or
mutate the task workspace.

## A7. Retry / resume semantics

A retry of the same durable Todo MUST use the exact claims and relative bounded
paths in `TaskResourceScopeSnapshot`. Only an authorized replan that creates a
new occurrence may produce a different snapshot.

Do not read a new live workset manifest to redefine an already-frozen child task.

`TaskOccurrenceProjection.WorksetBinding` remains the source used to create a
new snapshot; the persisted snapshot is the retry/resume authority.

Retry/resume validates the dynamic snapshot, binds the current allowed tool
subset, validates/binds `ResourceScopeSnapshot`, constructs one new attempt
envelope, emits the idempotent claim/unavailable diagnostics, and only then
performs cache lookup or transitions to `in_progress`. Any failure follows
A6.1 and starts no attempt.

## A8. External ExecutionWorld behavior

Do NOT change:

```go
acquireLocalExecutionWorldLease(ctx, leaseRoot)
```

in this phase.

Even when scheduler claims are disjoint, external/native providers using current whole-root snapshot delta attribution remain serialized by `LocalExecutionWorld`.

Add an explicit regression test proving this.

Later optimization requires a separate design with per-attempt scoped snapshots or mutation receipts.

## A9. Resource-claim observability

Emit bounded scheduling diagnostics; do not log secrets or file contents.

Emit this persisted content-free event once per occurrence revision, after the
initial `task_created` commit (or after an existing occurrence is rebound),
after the envelope is accepted, and before `TaskInProgress`:

```json
{
  "type": "resource_claims_resolved",
  "task_id": "...",
  "occurrence_revision": 1,
  "source": "authored+workset",
  "bounded_read_scope": true,
  "bounded_write_scope": true,
  "claims": [
    {"resource":"workspace:path:internal/team","mode":"write"}
  ]
}
```

Use an idempotency key derived from run ID, task ID, occurrence revision, and
`ResourceScopeSnapshot.Digest`. Emit the claims/source/flags directly from the
validated snapshot, not from a fresh derivation. The event reducer does not mutate task lifecycle;
it is an observability fact. JSON output/report/debug projections expose the
latest event for each task. Plain/TUI status may show one bounded summary line
but must not synthesize task state.

`source` is one of `authored`, `authored+workset`, or `runtime_fallback`. Sort
claims by resource then mode before hashing or emitting them.
Use `authored+workset` only when durable touched paths produced bounded
workspace claims, `runtime_fallback` when Hufu added a whole-root claim, and
`authored` only when no Hufu-owned workspace claim was required by a future
explicitly supported side-effect class.

Do not persist absolute home paths when a repository-relative resource is sufficient.

---

# 7. Phase A tests

Add or extend focused tests.

## A-T01 — legacy exact resource behavior

Existing tests must remain valid:

```text
read/read same generic resource -> compatible
read/write same generic resource -> conflict
default mode -> exclusive
```

## A-T02 — path hierarchy

Cover:

```text
internal/team vs internal/team/foo.go
internal/team vs internal/tools
"." vs any child
docs/a.md vs docs/b.md
```

## A-T03 — path normalization

Reject:

```text
../secret
/absolute/path
workspace:path:
```

Normalize safe equivalent paths to one canonical form.

Also cover:

```text
docs and docs/ -> workspace:path:docs
workspace:path:. -> valid whole-root claim
workspace:other:x -> rejected reserved namespace
unknown claim mode -> rejected
```

## A-T04 — workset read fan-out

Two read-only workset children with disjoint touched paths can run concurrently.

## A-T05 — workset write fan-out

Two write tasks:

```text
internal/team/a.go
docs/a.md
```

can run concurrently only when both are actually confined to those scopes.
They must not receive an implicit dependency from
`serializeConflictingMutationTasks`.

Also pair a confined reader of `internal/team/a.go` with a confined writer of
`docs/a.md`; they may overlap. Changing the reader to an unsupported/unbounded
surface gives it `workspace:path:. read` and must serialize it with that
writer.

Two overlapping or unbounded mutation tasks must receive the same durable
lower-index dependency ordering that `serializeMutationTasks` provided before
this feature.

## A-T06 — parent/child collision

Tasks whose effective workset claims are:

```text
internal/team
internal/team/a.go
```

must serialize when either is a writer.

The corresponding conflicting authored-claim case remains a static plan-lint
error, preserving current behavior.

## A-T07 — unbounded writer fallback

A workspace-writing task with no enforceable bounded paths gets:

```text
workspace:path:. exclusive
```

and serializes against any other workspace reader/writer claim as defined.

Include a reader with no `TouchedPaths`; its derived
`workspace:path:. read` claim must conflict with the writer.

## A-T08 — read/write escape denied

A task bound to:

```text
internal/team
```

attempts to write:

```text
internal/tools/x.go
```

The write MUST fail before mutation.

An enforced reader bound to the same scope must also reject a read of
`internal/tools/x.go` before opening it.

Repeat through every access path classified `PathScopeEnforced`, and prove
every `PathScopeDenied` reader/writer returns before starting its
process/handler. Unknown or custom workspace-access tools must make the task
unbounded during preflight.

## A-T09 — durable retry

Retry/resume obtains the exact effective claims from
`TaskResourceScopeSnapshot`. A changed live tool surface may narrow executable
authorization, but it cannot change the claim set; loss of required scope
enforcement blocks before dispatch.

## A-T10 — external provider isolation

Two external/native provider attempts on the same root remain serialized by `LocalExecutionWorld`, even if scheduler path claims are disjoint.

Run targeted race tests for scheduler/lease code.

## A-T11 — batch preflight atomicity

Place a valid task before a task with a malformed path claim or irreproducible
write scope. The whole batch must fail with no `task_created` event, no Todo,
zero cache lookup or lease acquisition, zero worker/model/action starts, and no
`TaskInProgress` transition. Orphaned decision-admission markers are allowed
because they are not executable task occurrences.

Repeat the failing case through the direct-agent single-task creation path.

## A-T12 — configured/runtime read/write-scope intersection

Cover ancestor, descendant, equal, disjoint, symlink, and absent-scope cases.
Configured ancestor + requested descendant and equal scopes succeed with the
requested scope. Configured descendant + requested ancestor, any disjoint
requested path, and an empty explicit scope are admission errors and must never
fall through to ordinary `AllowedPaths` behavior.

## A-T13 — backend eligibility

The same bounded workset task receives narrow claims on an eligible Hufu-local
tool surface and `workspace:path:. exclusive` on an external agent backend or
a surface containing an unsupported workspace reader or mutator.

## A-T14 — path authorization race

Attempt a concurrent symlink-parent replacement between validation and write.
Use an outside-root sentinel and an adversarial goroutine that repeatedly swaps
a parent directory and symlink. For each enforced structured writer, the
outside sentinel must remain byte-identical and every operation must either
modify the intended in-root regular file or fail before mutation. Run under
`-race`. Repeat the parent-swap case for enforced `view` reads. If a tool
cannot pass this test, classify its relevant access as
`PathScopeUnsupported` and prove the scheduler uses the whole-root fallback.

## A-T15 — resource-scope projection parity

Round-trip `TaskResourceScopeSnapshot` through task creation, every task
transition, event replay, session/checkpoint restore, task journal, projection
shadow, occurrence hashing, retry reset, and branch checkout. Tampering with a
claim/path/flag/digest blocks preflight. A legacy nil snapshot receives only
the whole-root compatibility scope. Two otherwise identical tasks with
different resource-scope digests cannot share task-cache or in-flight de-dup
results; old entries with an empty digest are stale.

---

# 8. Phase B — Stable Dynamic Tool Gateway

## B1. Goal

Reduce provider-visible schema churn caused by ordinary dynamic MCP inventory while keeping Hufu's authorization and lifecycle semantics unchanged.

### Current

```text
view
write
bash
github__get_issue
github__list_prs
k8s__get_pods
k8s__logs
...
submit_result
```

### Target v1

```text
view
write
bash
use_dynamic_tool
submit_result
```

where authorized MCP targets remain host-side logical tools.

This phase applies only to ordinary dynamic MCP tools.

## B2. Do not overload Hufu's existing "Capability" vocabulary

Hufu already uses capability terminology for:

- worker capability routing;
- preflight/environment capabilities.

Use a tool-specific name.

Provider-visible tool name:

```text
use_dynamic_tool
```

Use these internal names:

```text
DynamicToolGateway
DynamicToolTarget
DynamicToolCatalog
```

Avoid naming the new subsystem simply `CapabilityRegistry`.

## B3. Fixed gateway schema

Provider-visible schema MUST remain stable when MCP inventory changes.

Required v1 input shape:

```json
{
  "action": "search",
  "query": "github pull request"
}
```

`action` is exactly one of `search`, `inspect`, or `call`; the conditional
fields below define the other two object shapes.

Required fields:

```text
search  -> action
inspect -> action,target
call    -> action,target
```

`arguments` is required for `call`; `{}` is valid.

Do not dynamically insert target enums into the schema, because that recreates schema churn.

The exact v1 `fantasy.ToolInfo` contract is:

```text
name: use_dynamic_tool
description: Search, inspect, or call an authorized dynamic MCP tool. Search
             first when the exact target or arguments are unknown.
parameters:
  action:    {type: string, enum: [search, inspect, call]}
  query:     {type: string}
  target:    {type: string}
  arguments: {type: object, additionalProperties: true}
required: [action]
```

Conditional requirements are enforced by the handler because adding schema
branches would make provider compatibility less predictable:

```text
search  -> target forbidden; arguments forbidden; query optional
inspect -> target required; query forbidden; arguments forbidden
call    -> target required; arguments required; query forbidden
```

Reject unknown top-level gateway fields and more than one JSON document. Keep
the name, description, parameters, and required list byte-for-byte stable for
gateway schema version 1.

Reject the request before catalog lookup when the serialized gateway input
exceeds 256 KiB, `query` exceeds 512 Unicode code points, or `target` exceeds
256 bytes. Return a bounded structured error code; never echo arguments in the
error or audit stream.

All gateway-originated failures use `fantasy.NewTextErrorResponse` containing
one compact JSON object:

```json
{"error":{"code":"dynamic_invalid_request","message":"bounded diagnostic"}}
```

The closed v1 code set is `dynamic_input_too_large`,
`dynamic_invalid_request`, `dynamic_target_not_authorized`,
`dynamic_target_unavailable`, `dynamic_schema_invalid`,
`dynamic_policy_denied`, and `dynamic_transport_error`. Limit `message` to 512
runes after secret redaction. Preserve a target's MCP `IsError` state using the
existing direct-MCP result semantics rather than relabeling it as a gateway
parser failure.

The outer gateway error code is coarse; durable logical receipts retain the
existing stable underlying policy/authorizer reason code in `ReasonCode`.

Gateway mode is enabled by default for eligible manager-owned MCP targets on
new task occurrences; there is no second policy flag in v1. Compatibility
exceptions are limited to closed sequences, result-only protocol mode,
schema-ineligible/direct targets, agent-specific command tools, and legacy
resumed occurrences without a frozen dynamic snapshot.

## B4. Logical target identity

For v1, preserve existing MCP logical names as compatibility identity:

```text
<server>__<tool>
```

Example:

```text
github__get_issue
```

Catalog entries MUST retain:

```go
type DynamicToolTarget struct {
    Name        string
    Kind        string // "mcp"
    Server      string
    NativeName  string
    Description string
    InputSchema map[string]any
    Parameters  map[string]any
    Required    []string
    DescriptorSHA256 string
}
```

Do not expose credentials or raw transport configuration.

`DescriptorSHA256` is a descriptor fingerprint, not merely a JSON Schema
hash. Compute it from canonical JSON containing:

```text
gateway descriptor version
logical name
kind
server/native name
description
parameters
required fields
```

Sort all map keys and required-field lists before hashing. Description changes
are included because they affect gateway search and model behavior even when
the validation schema is unchanged.

Use these v1 helpers rather than ad hoc hashes at call sites:

```go
type mcpToolDescriptorV1 struct {
    Version     int            `json:"version"`
    Name        string         `json:"name"`
    Kind        string         `json:"kind"`
    ServerName  string         `json:"server_name"`
    NativeName  string         `json:"native_name"`
    Description string         `json:"description"`
    InputSchema map[string]any `json:"input_schema"`
}

type providerToolSchemaV1 struct {
    Version     int            `json:"version"`
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Parameters  map[string]any `json:"parameters"`
    Required    []string       `json:"required"`
}

func MCPToolDescriptorSHA256(tool MCPTool) (string, error)
func providerToolSchemaSHA256(info fantasy.ToolInfo) (string, error)
```

Each helper builds its fixed-field struct, clones and sorts every required
array inside the schema, validates that the schema can be JSON encoded,
marshals with Go's `encoding/json`, and returns lowercase
`hex(sha256(bytes))`.
Both `Version` fields are 1 and `mcpToolDescriptorV1.Kind` is exactly `mcp`.
`encoding/json` supplies deterministic string-map key order; do not hash `%v`,
pointer identity, handler type, live client state, credentials, or transport
addresses. Direct provider schemas use the second helper; manager MCP frozen
targets use the first.

## B4.1 Durable logical-tool snapshot

The current attempt catalog is not durable by itself. Add bounded metadata to
the task occurrence:

```go
const dynamicToolAuthorizationSnapshotVersion = 1

type FrozenDynamicToolTarget struct {
    Name              string `json:"name"`
    DescriptorSHA256  string `json:"descriptor_sha256"`
}

type DynamicToolAuthorizationSnapshot struct {
    Version             int                       `json:"version"`
    Targets             []FrozenDynamicToolTarget `json:"targets,omitempty"`
    FrozenCatalogDigest string                    `json:"frozen_catalog_digest"`
}
```

Add this exact field to `TodoItem`, `TodoSpec`, and
`TaskOccurrenceProjection`:

```go
DynamicToolAuthorization *DynamicToolAuthorizationSnapshot `json:"dynamic_tool_authorization,omitempty"`
```

`nil` means a legacy occurrence; every new occurrence stores a non-nil
version-1 value even when `Targets` is empty. Add and use one deep-clone helper
at every struct/event/projection boundary; callers must never share or mutate
the `Targets` backing array.

Like `ResourceScopeSnapshot`, this is runtime-owned Todo metadata and is not an
authorable `TaskDef` JSON/YAML field. Occurrence comparison obtains it only
from the prospective/durable Todo.

On admission and restore, require version 1, strictly ascending unique target
names, valid name/hash bounds, and a recomputed `FrozenCatalogDigest` match.
An invalid non-nil snapshot is `dynamic_snapshot_invalid` and blocks the
occurrence; it must not be treated as legacy.

Persist this snapshot in
`task_created`, task transition payloads, session/checkpoint projection, event
reducers, shadow comparison, and task journal data wherever the rest of the
immutable execution contract is carried. Include it in the occurrence digest.

The snapshot stores no schema body, handler, transport address, or credential.
On a new occurrence, capture target names and descriptor fingerprints from the
authorized catalog. On retry/resume:

1. resolve the live handler for every frozen name;
2. require an exact descriptor fingerprint match;
3. omit every live target not present in the frozen snapshot;
4. fail the occurrence closed if a frozen target required by a closed sequence
   is missing or changed;
5. for an ordinary target not required by a closed sequence that is missing or
   changed, remove it from the executable catalog, emit a bounded
   `dynamic_tool_unavailable` event, and do not replace it with a new target.

The content-free unavailable event schema is:

```json
{
  "type": "dynamic_tool_unavailable",
  "task_id": "...",
  "occurrence_revision": 1,
  "target": "github__get_issue",
  "reason": "missing"
}
```

`reason` is exactly `missing` or `descriptor_changed`.

Emit it at most once per task occurrence revision, target, and reason using an
idempotency key over those fields. Do not persist the schema body, live
descriptor, transport error, arguments, or credentials in this event.

`FrozenCatalogDigest` uses the section 12 length-prefixed record encoder over
`("dynamic_catalog_version", "1")`, followed by
`("target", name, fingerprint)` records in ascending name order. Each attempt derives
`ResolvedWorkerTools.LogicalToolsetDigest` from this frozen digest plus the
currently executable subset and current static restrictions. A missing
optional target therefore changes the attempt digest, while the frozen ceiling
still distinguishes this occurrence from one that never authorized that
target. Task-cache lookup and in-flight de-duplication use the attempt digest,
never the frozen digest alone.

Bounds for v1:

```text
maximum frozen dynamic targets per occurrence: 512
maximum logical target name: 256 bytes
descriptor hash: lowercase 64-character SHA-256 hex
maximum canonical gateway-eligible descriptor: 256 KiB
maximum schema nesting depth: 16
maximum aggregate schema nodes/properties/items: 2048
```

The target-count, logical-name, or hash bound applies to the occurrence
snapshot and exceeding it rejects new occurrence admission before
`task_created`. A manager target exceeding a descriptor/depth/node bound is
schema-ineligible and remains direct; it does not reject the task unless a
closed sequence or provider limit independently rejects that direct surface.
Legacy occurrences without this snapshot retain the old direct-MCP behavior
for compatibility, but they MUST NOT enable the gateway during resume. A new
occurrence is required to opt into gateway mode.

### B4.2 Creation and attempt binding

Add two explicit resolver operations:

```go
func (c *Coordinator) resolveNewTaskToolAuthorization(
    ctx context.Context,
    task TaskDef,
    def *agent.AgentDef,
) (*DynamicToolAuthorizationSnapshot, error)

func (c *Coordinator) bindFrozenTaskTools(
    ctx context.Context,
    task TaskDef,
    todo *TodoItem,
) (ResolvedWorkerTools, error)

func (m *MCPToolManager) SnapshotToolDescriptors() []MCPTool
```

`resolveNewTaskToolAuthorization` runs after the team-level MCP manager has
finished its existing load phase, but before task admission and
`task_created`. It uses `ResolveStaticWorkerTools` plus immutable copies of
manager descriptors. It does not require a Todo-bound concrete
`submit_result` handler and does not start a provider, model, worker, action,
or new MCP server. Agent-specific command tools are derived from their loaded
configuration but remain direct.

`SnapshotToolDescriptors` takes the manager read lock, deep-copies every
parameter/schema map and required-field slice, sorts by logical name, and
returns no client/transport handles. Returning the manager's backing slice or
maps is forbidden because an attempt catalog must remain immutable while
other tasks resolve tools.

The snapshot is an occurrence-wide authorization ceiling, not the tool surface
of one lifecycle turn. It contains every authorized manager-owned MCP target,
including schema-ineligible or closed-sequence targets that remain direct; it
does not contain built-ins, protocol tools, or agent-specific command tools.
For an ordinary task, capture manager targets authorized in normal mode. For
`PlanFirst`, capture the union of manager targets statically authorized across
its reachable initial-plan and approved-plan modes. Closed sequence and
result-only projection still decide what is visible in each turn. This union
is frozen before execution; plan text cannot add a target.

For a batch, reserve IDs and build prospective Todos, then resolve every new
snapshot before occurrence admission or `task_created`. Attach it to
`TodoSpec`, then include it in occurrence admission. If snapshot creation for
any task fails, create no Todos and start no work; reserved numeric IDs may be
consumed but do not identify executable occurrences.

`bindFrozenTaskTools` is the attempt-time operation used by
`TaskExecutionEnvelope`. It constructs concrete handlers, intersects the live
manager catalog with the frozen snapshot, verifies descriptor fingerprints,
reapplies the current lifecycle's static restrictions, projects direct versus
gateway-visible tools, and returns the final provider and logical surfaces. It
never expands the frozen logical authorization.

For initial admission, this method accepts the prospective `TodoItem` built
from the reserved ID even though it is not yet present in `TodoList`. Construct
task-bound protocol handlers with that reserved ID, but do not invoke them.
Keep the existing public `ResolveTaskTools` guard that rejects arbitrary
unknown Todo IDs; expose the prospective path only through the coordinator's
private batch-admission resolver. After all envelopes pass,
`CommitTaskCreationResolved` must persist the byte-equivalent candidate
occurrences. A commit failure discards every precomputed envelope.

`DynamicAuthorization` retains the immutable snapshot pointer, including
temporarily unavailable non-sequence targets. `DynamicTargets` and
`AuthorizedNames` contain only targets executable in the current attempt.
`LogicalToolsetDigest` is the attempt digest defined in section 12 and must
include both `FrozenCatalogDigest` and this executable projection.

## B5. Split provider surface from authorization surface

Current `ResolvedWorkerTools` is documented as one source for both model-visible names and runtime allowlist. That is no longer sufficient.

Extend it explicitly rather than silently changing `Names`.

During resolution, retain tool provenance (`base`, `manager_mcp`,
`agent_command`, or `protocol`) alongside each candidate until projection is
complete. Do not infer dynamic eligibility from a `server__tool` spelling or
from the final name alone. A duplicate provider-visible/logical name backed by
different handlers or origins is an admission error; do not let first-seen map
or slice order select an implementation.

Required v1 shape:

```go
type ResolvedWorkerTools struct {
    // Existing concrete provider-visible handlers.
    Tools []fantasy.AgentTool

    // Provider-visible names matching Tools.
    Names []string

    // Exact logical targets authorized for this attempt.
    // Includes direct tools and proxied dynamic targets.
    AuthorizedNames []string

    // Optional dynamic target catalog limited to AuthorizedNames.
    DynamicTargets []DynamicToolTarget

    // Existing compatibility/description field.
    Capabilities []string

    // Deterministic digests for cache/telemetry.
    ProviderSurfaceDigest string
    LogicalToolsetDigest  string

    // Durable assertion used to prevent retry/resume widening.
    DynamicAuthorization *DynamicToolAuthorizationSnapshot

    // Pre-wrapper workspace access behavior by authorized logical name.
    WorkspaceScopeDescriptors map[string]tools.ToolWorkspaceScopeDescriptor
}
```

Use these field meanings in v1. Additional private fields are allowed, but do
not rename or collapse the provider surface, logical authorization, dynamic
catalog, snapshot, or digest concepts during implementation.

### Migration rule

When gateway mode is not involved:

```text
AuthorizedNames == Names
DynamicTargets == nil
```

This minimizes semantic change.

Provider order is part of prompt-cache stability. Build `Tools` and `Names` in
this deterministic order:

1. existing direct built-in/base tools in their current canonical order;
2. direct supplemental tools sorted by exact provider-visible name;
3. `use_dynamic_tool`, when present;
4. `submit_plan` or `submit_result`, when required.

For a closed `ToolSequence`, preserve the existing sequence projection exactly
instead of applying this general order. Sort `AuthorizedNames` independently
for hashing and membership; never rely on MCP manager load/goroutine order.

## B6. Static resolution remains authoritative

`ResolveStaticWorkerTools` continues returning the authorized **logical names**.

Then runtime projection separates:

```text
authorized logical targets
        │
        ├─ direct provider-visible tools
        └─ dynamic proxied targets
                   ↓
          use_dynamic_tool
```

Do not move allow/deny policy into the gateway.

For a newly admitted occurrence, static resolution and dynamic snapshot
creation happen before `task_created`. For an existing occurrence, static
resolution may narrow the frozen set because a tool is now denied or
unavailable, but it may never add a live MCP name that is absent from the
snapshot.

## B7. Which tools are proxied in v1

Proxy:

```text
ordinary MCPToolManager-managed tools loaded as supplemental tools
```

Keep direct:

```text
built-in Hufu worker tools
submit_result
submit_plan
all tools in a closed Execution.ToolSequence
result-repair/resume protocol surface
```

Agent-specific `AgentMCPServer` tools remain direct in v1. Despite their
historical name, they are in-process command wrappers with different identity,
dispatch, and confinement semantics; they are not
`MCPToolManager.ExecuteTool` transport targets.

An MCP target whose input schema cannot be validated by the v1 validator also
remains direct. Stable gateway projection applies only to eligible targets; it
must never weaken validation to hide one more schema.

Do not add lazy MCP startup in this phase.

Reserve `use_dynamic_tool`. A built-in, custom, agent-specific, or MCP tool
with that logical/provider-visible name is an admission error while gateway
mode is enabled; do not silently shadow either handler.

## B8. Lifecycle rules

### Normal mode

If at least one authorized dynamic target exists:

```text
add use_dynamic_tool
hide those individual dynamic target schemas
```

`Names` contains `use_dynamic_tool` plus direct tools.
`AuthorizedNames` contains the same direct tools, `use_dynamic_tool`, and the
exact proxied logical target names. Including the gateway name permits the
outer provider-visible handler to run; it does not authorize any target.

### Initial plan

Preserve current plan surface semantics.

Do not introduce the gateway if current policy would not expose those MCP targets.

### Approved plan

Same authorization logic as current code; only representation changes for eligible ordinary dynamic tools.

### Result repair / resume

Preserve current invariant:

```text
provider-visible surface == [submit_result]
```

No gateway.

Here "resume" means result-protocol repair mode. Crash-resume of an ordinary
worker retains its normal lifecycle surface but intersects live MCP handlers
with the durable dynamic authorization snapshot from B4.1.

### Closed ToolSequence

Any exact tool named by the sequence remains direct in v1.

Do not teach the sequence validator that a proxy call "means" another tool in this PR series.

---

# 9. Gateway operations

## B9.1 search

Input:

```json
{
  "action": "search",
  "query": "github pull request"
}
```

Rules:

- search only `DynamicTargets` authorized for this attempt;
- normalize query, target, and description with `strings.ToLower`; split with
  `unicode.IsLetter` / `unicode.IsNumber` boundaries, de-duplicate tokens, and
  discard empty tokens;
- assign one mutually exclusive name score: exact normalized target-name match
  100, target-name prefix 60, or any target-name token/substring match 30;
- add 10 for each distinct query token present in the normalized description,
  capped at 40 description points;
- for a non-empty token set, omit zero-score targets;
- sort by score descending and then exact logical target name ascending;
- return at most 12 results;
- return:
  - exact target;
  - description truncated to 512 runes;
  - kind;
- do not return full schemas.

An empty query or a query yielding no tokens returns the first 12 authorized
targets in ascending target-name order.

Return canonical compact JSON with this exact shape and key names:

```json
{"results":[{"name":"github__get_issue","description":"Get one issue","kind":"mcp"}]}
```

No match returns `{"results":[]}` as a successful response.

## B9.2 inspect

Input:

```json
{
  "action": "inspect",
  "target": "github__get_issue"
}
```

Return:

```text
name
description
required fields
input schema
descriptor SHA-256
```

The untruncated compact JSON keys are exactly `name`, `description`,
`required`, `input_schema`, `descriptor_sha256`, `schema_truncated`; set
`schema_truncated` to `false`. The truncated form omits `input_schema`, sets
`schema_truncated:true`, and adds `required_truncated` only when true.

Only for an authorized target.

Apply a bounded output limit to schema rendering.

The rendered inspect response is canonical JSON and is limited to 32 KiB. If
the complete schema cannot fit, omit the schema and return the name,
descriptor SHA-256, as many required-field names as fit, plus
`schema_truncated:true` and `required_truncated:true` when applicable. Measure
the final encoded bytes and drop trailing required names until the object fits;
truncate the displayed description to 4096 runes before sizing. Never
byte-slice encoded JSON or return invalid truncated JSON.

## B9.3 call

Input:

```json
{
  "action": "call",
  "target": "github__get_issue",
  "arguments": {
    "owner": "kjelly",
    "repo": "hufu",
    "issue_number": 123
  }
}
```

Dispatch sequence MUST be:

```text
1. resolve target from this attempt's frozen authorized catalog
2. reject unknown / stale / unauthorized target
3. validate arguments against the stored schema
4. run current Hufu target-specific authorization
5. run current MCP ToolAuthorizer
6. invoke MCP transport
7. capture receipt/audit using logical target identity
8. return bounded tool response
```

Do not treat permission to call `use_dynamic_tool` as permission to call every target.

The catalog object held by the gateway is an immutable attempt-local copy.
`search` and `inspect` never query the manager's live inventory. `call` uses
the frozen descriptor to validate input, then resolves the live handler by
logical name and requires its descriptor fingerprint to still match before
transport execution.

Gateway output uses the same tool-result normalization/redaction boundary as
direct MCP execution and is capped at 256 KiB. Truncation must preserve valid
UTF-8 and add an explicit marker. Transport error state must remain an error;
truncation must not turn it into success.

The marker is `\n...[truncated by hufu: original_bytes=N,
limit_bytes=262144]`. Reserve marker bytes first, retain the largest valid UTF-8
prefix that keeps the final response at or below 262144 bytes, then append the
marker. Record truncation in the logical invocation receipt without storing
the discarded content.

---

# 10. Gateway authorization details

## B10.1 Effective allowlist

Any context currently built from:

```go
resolvedTools.Names
```

for runtime authorization MUST be reviewed.

Where the intent is "what may execute", use:

```go
resolvedTools.AuthorizedNames
```

Where the intent is "what schemas are sent to the model", use:

```go
resolvedTools.Names
```

Add tests to prevent these concepts from drifting together again.

Apply the split consistently:

| Consumer | Field |
|---|---|
| Fantasy/provider tool list and context-window schema estimate | `Tools` / `Names` |
| `withEffectiveToolsAllowedForTask`, MCP authorizer, logical permission checks | `AuthorizedNames` |
| prompt text naming directly callable tools | `Names` |
| prompt/help text describing dynamic capability | gateway instructions plus bounded `search`; never inject the full catalog |
| closed `ToolSequence` validation | direct `Tools` / `Names` only |
| task cache and in-flight de-dup | envelope `LogicalToolsetDigest` + `ResourceScopeDigest` |
| telemetry for provider schema stability | `ProviderSurfaceDigest` |

Update the hufu-local backend assertion so it compares provider names/tools,
authorized logical names, frozen snapshot, and both digests with the canonical
`bindFrozenTaskTools` result. Comparing only `Names` is insufficient.

The provider-visible gateway handler passes the ordinary outer
`policyGatedTool` check using the literal `use_dynamic_tool` entry. For
`search` and `inspect`, that outer check is sufficient because neither action
executes a target. For `call`, the gateway must additionally invoke the shared
logical-target gate below. The outer gateway check must not consume a closed
`ToolSequence` slot; gateway mode is disabled whenever a closed sequence is
active.

Update `artifactScopeToolDenial`, `readOnlyToolMutation`, and
`isReadOnlyToolCall` handling so the wrapper does not classify the gateway name
itself as an unknown mutator. The outer wrapper authorizes only access to the
gateway and lets its handler parse the action: `search` / `inspect` are
read-only metadata, while `call` always reaches the shared logical-target gate
where artifact scope and `side_effect:none` are evaluated against the exact
target. A malformed action fails in the gateway parser and reaches neither
target policy nor transport. This exception is keyed by the reserved concrete
gateway handler type/provenance, not merely by a caller-controlled tool name.

## B10.2 MCP authorizer

The gateway call path MUST reuse the existing MCP authorization semantics currently reached by `mcpAgentTool.Run`.

Do not call `MCPToolManager.ExecuteTool` in a way that skips:

```text
ToolAuthorizer
```

Extract a shared authorized execution method from `mcpAgentTool.Run`; do not
duplicate policy.

Required v1 manager method:

```go
func (m *MCPToolManager) ExecuteAuthorizedTool(
    ctx context.Context,
    logicalName string,
    expectedDescriptorSHA256 string,
    input string,
) (string, bool, error)
```

It must:

1. resolve the current manager-owned descriptor by logical name;
2. compare it with the expected frozen descriptor fingerprint supplied by the
   caller;
3. invoke the context `ToolAuthorizer` with server/native name and the exact
   input;
4. execute transport only after authorization succeeds;
5. preserve current error/result semantics and redaction.

The direct `mcpAgentTool.Run` path must call the same method, proving direct
and proxied MCP calls cannot drift. A new occurrence's direct manager target
passes its frozen fingerprint; a legacy direct handler passes the fingerprint
of its own immutable `MCPTool` copy. An empty expected fingerprint is an error,
not a request to skip comparison.

## B10.3 tools.CheckToolPermissionDetail

If the dynamic target is represented in Hufu's normal tool allowlist, check the **logical target**, not merely `use_dynamic_tool`.

The gateway itself can be model-visible without becoming an authorization wildcard.

Do not call `tools.CheckToolPermissionDetail` as a substitute for the rest of
`policyGatedTool.Run`. Extract a coordinator-owned logical-target gate shared
by direct and gateway execution. For a gateway `call`, it must apply, in this
order:

```text
frozen AuthorizedNames membership
gateway argument validation
team/task deny and trusted-grant policy
tools.CheckToolPermissionDetail(logical target)
artifact/workset scope policy for the logical target
side_effect:none mutation classification
decision checkpoint / commit gate
MCP ToolAuthorizer
transport
```

Closed-sequence enforcement is intentionally absent here because sequence
targets remain direct in v1. Skill-load and protocol-only rules are also not
applicable to ordinary MCP targets. Every other direct-MCP policy hook must
remain shared or have an explicit parity test.

Refactor `artifactScopeToolDenial` so direct and proxied MCP paths can supply
the same logical identity/provenance descriptor without requiring access to an
unexported `mcpAgentTool` value. The gateway must not default this check to
"supported" merely because its outer wrapper is Hufu-owned.

The gateway meta-tool must be classified separately from its target:

- `search` and `inspect` are read-only host metadata operations;
- `call` takes the side-effect classification of the exact logical target;
- an unknown MCP target is mutation-capable for `side_effect:none`, preserving
  today's fail-closed behavior unless a future typed MCP effect contract is
  added.

## B10.4 force-mcp

`--force-mcp` must continue to:

- suppress the same built-in execution/network tools;
- permit authorized MCP execution;
- not grant all MCP targets.

Add a regression test.

---

# 11. Argument validation

Current direct MCP tools expose their JSON schemas to Fantasy/provider tooling.

After proxying, Hufu itself MUST validate `arguments` before MCP transport.

Requirements:

1. use the target's captured schema;
2. reject malformed non-object arguments;
3. reject missing required fields;
4. reject schema-invalid types where current schema representation supports validation;
5. perform validation before permission hooks that would otherwise authorize a malformed operation;
6. return a structured, bounded diagnostic;
7. do not execute transport on validation failure.

Implement the closed subset below as a small local validator; do not add a
general JSON Schema dependency in v1.

### V1 schema eligibility

Capture the complete MCP input schema needed to reproduce the current Fantasy
`ToolInfo` projection, not only an untyped target-name map. Canonicalize it
before fingerprinting.

Extend the existing manager descriptor without removing its compatibility
projection:

```go
type MCPTool struct {
    // existing Name, Description, Parameters, Required, ServerName, OrigName
    InputSchema map[string]any
}
```

At MCP discovery, choose `RawInputSchema` when present; otherwise marshal the
typed `InputSchema`. Decode exactly one JSON object with `UseNumber`, deep-copy
it into `MCPTool.InputSchema`, and retain the existing `Parameters` /
`Required` derivation for direct Fantasy tools. Do not change the direct
provider schema as a side effect of capturing richer validation metadata.

The v1 validator supports:

```text
root type=object
properties
required
additionalProperties as a boolean
type: string, number, integer, boolean, object, array, null
enum and const
nested object properties/required/additionalProperties
array items
```

At every schema node, the exact v1 keyword allowlist is `type`, `properties`,
`required`, `additionalProperties`, `enum`, `const`, `items`, plus the ignored
annotation keywords `description`, `title`, `default`, and `examples`. `type`
must be one supported string, `items` must be one schema object, and
`additionalProperties` must be absent or boolean. Any other assertion,
applicator, composition, reference, or conditional keyword—including numeric
or string bounds, `pattern`, `$ref`, `$defs`, `allOf`, `anyOf`, `oneOf`, `not`,
conditionals, tuple items, or non-boolean `additionalProperties`—makes the
target schema-ineligible for gateway proxying in v1. Keep that target direct
rather than silently ignoring a constraint. Annotation values remain visible
to `inspect` but do not affect validation.

Use `json.Decoder` with exactly one JSON value. `call.arguments` must decode to
an object; reject trailing JSON, non-object values, missing required keys,
unsupported types, integer/non-integer mismatches, enum/const mismatches, and
unknown fields when `additionalProperties:false`. Validation failure performs
zero permission callbacks and zero transport calls.

Decode numbers as `json.Number`. Validate `integer` by parsing with
`math/big.Rat` and requiring denominator 1; do not round through `float64`.
For `enum` / `const`, compare JSON values recursively with object key order
ignored, array order preserved, and numeric values equal when their exact
rational values are equal (so `1` equals `1.0`).

After validation, encode `arguments` once with `encoding/json` into compact,
deterministic key order and pass that exact byte string to the logical policy
gate, MCP `ToolAuthorizer`, audit redaction boundary, and transport. Never pass
the outer gateway object as the target's input, and do not re-encode separately
at each gate.

---

# 12. Tool-surface digests and cache correctness

Hufu already carries `ToolRegistryVersion` in task-cache identity.

The gateway introduces two distinct identities:

## Provider surface digest

Hash:

```text
ordered direct provider-visible tool schema fingerprints
+ fixed use_dynamic_tool schema version
```

A provider-visible schema fingerprint is SHA-256 over canonical JSON of tool
name, description, parameters, and required fields. Sort tools by name before
hashing; do not depend on MCP map iteration or construction order. Include one
explicit `gateway_schema_version=1` record when the gateway is present.

Expected property:

Adding a new ordinary authorized MCP target does **not** change the gateway schema itself.

## Logical toolset digest

Hash:

```text
ordered authorized logical target names
+ logical target descriptor fingerprints
+ relevant static policy version
```

This digest includes every direct authorized name and descriptor fingerprint,
every frozen dynamic target name and descriptor fingerprint, lifecycle mode,
effective closed sequence, `no-net`, `force-mcp`, team denies, and trusted task
grants. It also includes the snapshot's `FrozenCatalogDigest` and the currently
executable dynamic subset, so a missing optional live handler cannot collide
with either its fully available attempt or an occurrence that never had the
authorization. Canonicalize these as sorted length-delimited records before
SHA-256 hashing; do not concatenate ambiguous delimiter-containing strings.

The v1 encoder writes each record part as an unsigned 64-bit big-endian byte
length followed by the UTF-8 bytes. Hash records in this exact order:

```text
logical_toolset_digest_version, "1"
frozen_catalog_digest, <digest or "legacy">
direct, <name>, <provider descriptor fingerprint>       (name-sorted)
frozen_dynamic, <name>, <descriptor fingerprint>        (name-sorted)
executable_dynamic, <name>, <descriptor fingerprint>    (name-sorted)
lifecycle_mode, <mode>
workflow_phase, <phase>
side_effect, <canonical SideEffectClass>
sequence, <zero-based decimal index>, <logical name>     (original order)
no_net, <true|false>
force_mcp, <true|false>
team_deny, <logical name>                                (name-sorted)
trusted_task_grant, <logical name>                       (name-sorted)
```

The provider descriptor fingerprint is
`providerToolSchemaSHA256(tool.Info())`. Add a digest-version constant next to
the encoder and require a version bump whenever policy semantics add another
authorization input.

Expected property:

Changing the authorized MCP tool set or target schema DOES invalidate semantic task-cache freshness where the existing `ToolRegistryVersion` contract requires it.

Do not optimize prompt caching by making task-result cache stale.

Required cache rule:

```text
ToolRegistryVersion remains based on logical authorization semantics,
not provider-visible schema count alone.
```

Expose provider-surface digest separately for telemetry.

### Cache wiring

The current `ComputeCacheIdentity` hashes only coordinator `coreTools` names;
that is insufficient for this feature. Extend the execution-cache request and
store contracts with both preflight envelope digests:

```go
type TaskCacheLookupRequest struct {
    // existing fields...
    LogicalToolsetDigest string
    ResourceScopeDigest  string
}

type TaskCacheStoreRequest struct {
    // existing fields...
    LogicalToolsetDigest string
    ResourceScopeDigest  string
}
```

For `TaskCacheLookupExecution`, set `CacheIdentity.ToolRegistryVersion` from
this non-empty logical digest and add
`CacheIdentity.ResourceScopeDigest string` from the non-empty snapshot digest.
Store both values with the result. Do not recompute either from live MCP
inventory or current task configuration inside `ComputeCacheIdentity`.

The DAG scheduler must resolve task execution envelopes before task-cache
lookup, then pass both digests to lookup and store. Include both in the
in-flight de-duplication key; two otherwise identical tasks with different
logical authorization or resource scopes must not share one execution result.

Pre-occurrence semantic duplicate detection may compute both candidate digests
using the same static resolvers. If it cannot do so without starting a
provider, MCP process, model, or action, skip cross-run cache reuse for that
candidate; never substitute the old core-only hash or an empty resource digest.

Compatibility rule for restored cache/journal entries:

```text
new lookup has non-empty logical digest + old entry has empty/core-only digest
    -> stale, no cache hit
new lookup has non-empty resource-scope digest + old entry has empty digest
    -> stale, no cache hit
```

Provider surface digest is telemetry only and MUST NOT replace the logical
digest in task-result cache identity.

---

# 13. Receipts, audit and observability

A proxied call must not appear to evidence consumers as merely:

```text
tool = use_dynamic_tool
```

Persist both:

```text
gateway_tool = use_dynamic_tool
logical_tool = github__get_issue
kind = mcp
```

Where an existing receipt consumer accepts one canonical tool identity, the
canonical identity MUST be the logical target; the gateway is auxiliary
metadata.

Do not break:

- execution receipts;
- evidence manifest;
- audit export;
- decision witness;
- replay.

Add only bounded metadata.

Add this attempt-local coordinator callback to the gateway context; do not let
`internal/mcp` import `internal/team`:

```go
type DynamicToolInvocationEvent struct {
    Phase         string // authorized, denied, started, finished
    GatewayTool   string
    LogicalTool   string
    ToolCallID    string
    DescriptorSHA256 string
    IsError       bool
    ReasonCode    string
}

type DynamicToolInvocationReporter func(DynamicToolInvocationEvent)
```

The gateway emits `authorized/denied` around the logical policy gate and
`started/finished` around transport. Do not include raw arguments or results in
this event. The coordinator uses the existing redacted/bounded values already
available at the call boundary when writing human diagnostics.

Extend `ExecutionReceipt` with this bounded logical invocation projection:

```go
type ToolInvocationReceipt struct {
    GatewayTool       string `json:"gateway_tool,omitempty"`
    LogicalTool       string `json:"logical_tool"`
    ToolCallID        string `json:"tool_call_id,omitempty"`
    DescriptorSHA256 string `json:"descriptor_sha256,omitempty"`
    Outcome           string `json:"outcome"` // denied, error, success
    ReasonCode        string `json:"reason_code,omitempty"`
    Truncated         bool   `json:"truncated,omitempty"`
    OriginalBytes     int    `json:"original_bytes,omitempty"`
}

type ExecutionReceipt struct {
    // existing fields...
    ToolInvocations          []ToolInvocationReceipt `json:"tool_invocations,omitempty"`
    ToolInvocationsTruncated int                     `json:"tool_invocations_truncated,omitempty"`
}
```

Create logical invocation receipts only for `call`; `search` and `inspect`
remain outer gateway observations because they execute no logical target.
Limit this list to the effective positive per-attempt tool-step budget capped
at 256; if no positive budget is available, use 256. Retain the first entries
in execution order and increment `ToolInvocationsTruncated` for each omitted
entry. Clone the slice at all receipt ingress/egress and shadow projection
boundaries.

Provider callbacks continue to record the outer `use_dynamic_tool` call. The
logical reporter additionally updates, with parent `ToolCallID` correlation:

- task transcript;
- audit log;
- status/event stream;
- retry evidence and last-operation identity;
- skill-pattern detection, if enabled;
- execution receipt.

Do not feed the synthetic logical event back through Fantasy's provider
callbacks, and do not count one gateway call as two model tool steps. Display
may render it as `use_dynamic_tool → github__get_issue`, but durable canonical
tool identity for the executed operation is `github__get_issue`.

Optional implementation diagnostics (omission does not block a stage or the
Definition of Done):

```text
provider_tool_schema_count
dynamic_tool_target_count
provider_tool_surface_digest
logical_toolset_digest
dynamic_tool_search_count
dynamic_tool_inspect_count
dynamic_tool_call_count
dynamic_tool_denied_count
```

No tool arguments or secrets in metrics.

---

# 14. Phase B tests

## B-T01 — no dynamic targets

No MCP dynamic targets:

```text
no use_dynamic_tool
existing provider-visible surface unchanged
```

## B-T02 — many MCP tools, one gateway

With 50 ordinary authorized MCP tools:

```text
provider receives one use_dynamic_tool schema
not 50 individual MCP schemas
```

except any tool explicitly required by closed `ToolSequence`.

## B-T03 — search authorization

Search returns only authorized targets.

Denied/agent-ineligible targets are absent.

## B-T04 — inspect authorization

Inspecting an unauthorized target fails closed.

## B-T05 — call authorization

Calling an unauthorized target does not reach MCP transport.

Prove denial at each shared logical-target gate: frozen snapshot membership,
team/task deny, interactive permission, side-effect boundary, decision commit
gate, and MCP `ToolAuthorizer`.

## B-T06 — schema validation

Malformed arguments fail before MCP transport.

Cover trailing JSON, non-object input, required fields, primitive/nested types,
integer validation, arrays, enum/const, and
`additionalProperties:false`. A schema using an unsupported structural keyword
keeps its direct tool instead of appearing in the gateway catalog.

## B-T07 — current MCP authorizer preserved

A target denied by the existing `ToolAuthorizer` is still denied through the gateway.

## B-T08 — force-mcp

`--force-mcp` works with the gateway and does not widen the MCP allowlist.

## B-T09 — result-protocol repair

Result repair / result-only resume remains exactly:

```text
[submit_result]
```

## B-T10 — initial plan

Existing initial-plan tool-surface tests remain valid.

## B-T11 — closed sequence

A dynamic MCP tool explicitly referenced by `Execution.ToolSequence` remains direct and sequence validation remains byte-for-byte semantically equivalent.

## B-T12 — cache invalidation

Changing logical MCP target inventory/schema changes logical toolset identity even when provider-visible gateway schema stays unchanged.

Also prove cache lookup/store and in-flight de-duplication do not share results
across distinct logical digests, and an old empty/core-only digest is stale
against a new non-empty digest.

## B-T13 — receipt target identity

Evidence records the exact logical MCP tool, not only the gateway.

Assert transcript, audit, status event, retry evidence, last operation, skill
detector, receipt cloning, and JSON/report projections. The outer and logical
records share one parent call ID and consume one model tool step.

## B-T14 — provider-surface stability

Two otherwise identical worker requests with different ordinary MCP inventories have the same `use_dynamic_tool` schema fingerprint.

## B-T15 — durable retry/resume authorization

Create an occurrence with targets A and B, then resume against live inventory
A, B, C. C must remain undiscoverable and uncallable. Removing B makes it
unavailable without adding C. Changing A's descriptor fingerprint makes A
unavailable for an ordinary attempt. A closed sequence requiring
missing/changed B fails closed before a model call.

Legacy occurrences without a dynamic snapshot use the old direct surface and
never enable the gateway.

## B-T16 — snapshot projection parity

Round-trip `DynamicToolAuthorizationSnapshot` through task creation, every
task transition, event replay, session/checkpoint restore, task journal,
projection shadow, occurrence hashing, retry reset, and branch checkout.

## B-T17 — non-manager and collision behavior

Agent-specific `AgentMCPServer` command tools and schema-ineligible MCP tools
remain direct. Any competing `use_dynamic_tool` name fails admission before a
provider request. Two different handlers/origins with the same logical or
provider-visible name also fail admission deterministically.

## B-T18 — gateway meta-action safety

`search` and `inspect` work for a `side_effect:none` task when the gateway is
authorized, while `call` applies the exact target's current fail-closed
side-effect classification. A closed `ToolSequence` exposes no gateway and
consumes exactly its existing direct slots.

---

# 15. Implementation sequencing

Do not implement all behavior in one large commit.

## PR/Stage 1 — Resource claim primitives

Behavior:
- add workspace path namespace;
- add canonical parser/normalizer;
- hierarchical conflict tests;
- keep existing scheduler behavior otherwise unchanged.

Files likely touched:

```text
internal/team/coordinator.go
internal/team/plan_revision.go
internal/team/resource_claim_test.go (existing; extend, do not treat as new —
it already covers TestResourceClaimModes / TestDAGSchedulerResourceConflict)
```

Acceptance:

```bash
go test ./internal/team -run 'ResourceClaim|DAGSchedulerResource'
go test -race ./internal/team -run 'ResourceClaim|DAGSchedulerResource'
```

## PR/Stage 2 — Durable tool authorization + surface data-model split

Behavior unchanged for users.

Add explicit distinction:

```text
provider-visible tools
authorized logical tools
dynamic target catalog
digests
durable dynamic authorization snapshot
```

Keep all tools direct in this stage. Existing no-MCP and MCP provider surfaces
must remain identical while task creation/resume begins freezing and enforcing
the logical authorization snapshot.

Files likely touched:

```text
internal/team/dynamic_tool_authorization.go (new; snapshot/catalog data only)
internal/team/services.go
internal/team/static_tool_resolution.go
internal/team/subagent_provider.go
internal/team/coordinator_task_run.go
internal/team/coordinator_declared_tool_runner.go
internal/team/coordinator_run.go
internal/team/coordinator_execute.go
internal/team/task_occurrence_projection.go
internal/team/coordinator_eventstore.go
internal/team/event_reducers.go
internal/team/status.go
internal/team/projection_shadow.go
internal/team/coordinator_session.go
internal/team/task_journal.go
internal/team/coordinator_taskcache.go
internal/team/cache_policy.go
internal/mcp/manager.go
```

Acceptance:

- B-T12, B-T15, and B-T16 with direct MCP tools still exposed;
- old occurrence/cache compatibility tests;
- result-repair and closed-sequence surface tests unchanged.

Do not enable proxying or narrow writer concurrency until this stage is green.

## PR/Stage 3 — Execution envelopes + resource-scope durability + scheduler integration

This stage lands the envelope/snapshot/scheduler plumbing only. It
deliberately does **not** include A5's read/write confinement enforcement or
A5.1's tool-behavior taxonomy (Stage 4 below). Without A5.1, no tool is yet
classified `PathScopeEnforced`, so A4's own derivation rules make every task
fall through to the conservative whole-root claim
(`workspace:path:. read`/`exclusive`) exactly as they do before this spec.
Observable scheduling concurrency is therefore unchanged at the end of this
stage — only the internal envelope/snapshot/scheduler machinery is added —
mirroring Stage 2's "behavior unchanged for users" landing pattern.

Behavior:
- resolve every initial-batch execution envelope atomically before dispatch (A4, A6.2);
- freeze and persist `TaskResourceScopeSnapshot` before `task_created` (A4.1);
- add effective runtime claims derived from frozen `WorksetBinding`, using the
  whole-root fallback rules in A4 (SideEffectNone/SideEffectWorkspaceWrite/etc.)
  since no tool is enforceable yet;
- add the generic path-scope intersection helpers (`IntersectWritePathScopes`,
  `IntersectReadPathScopes`, `IntersectPathScopeCeilings`) as pure functions,
  without yet wiring them into `cfgWithMergedPaths` (Stage 4 flips the
  union-to-intersection switch once enforcement exists to make the
  intersection meaningful);
- wire the scheduler's active-claim tracking to the precomputed envelope (A6);
- add retry/resume envelope reproduction (A7) and resource-claim
  observability (A9);
- preserve `LocalExecutionWorld` lease and keep external agent backends
  whole-root (A8).

Files likely touched:

```text
internal/team/coordinator.go
internal/team/dag_scheduler.go
internal/team/delegation_policy.go
internal/team/workset.go
internal/team/services.go
internal/team/coordinator_run.go
internal/team/coordinator_execute.go
internal/team/coordinator_taskcache.go
internal/team/task_occurrence_projection.go
internal/team/coordinator_eventstore.go
internal/team/event_reducers.go
internal/team/status.go
internal/team/projection_shadow.go
internal/team/coordinator_session.go
internal/team/task_journal.go
internal/team/coordinator_task_execution_envelope.go (new; see §2.2 rule 3)
internal/team/coordinator_resource_scope.go (new; see §2.2 rule 3)
```

Acceptance:

- A-T01–A-T03, A-T06, A-T07, A-T09, A-T11, A-T13, A-T15 from the Phase A test
  matrix, run against the whole-root-fallback behavior this stage actually
  produces (A-T04/A-T05/A-T08/A-T12/A-T14, which require enforced narrow
  claims, are deferred to Stage 4's acceptance below);
- zero-start batch-preflight test (A-T11);
- cache/in-flight de-dup digest tests;
- targeted scheduler and `LocalExecutionWorld` race tests (A-T10).

## PR/Stage 4 — Read/write confinement enforcement (TOCTOU-safe scoped file access)

This is a separate, security-sensitive stage: it is what actually unlocks
narrow bounded-writer concurrency, by making tools capable of enforcing the
scope Stage 3 already computes. Track it with its own review and its own
race-test sign-off; do not merge it as part of Stage 3.

Behavior:
- add the `ToolWorkspaceScopeDescriptor`/`PathScopeBehavior` taxonomy and
  per-constructor descriptors in `internal/tools` (A5.1);
- implement the shared `os.Root`-based scoped file-access helper described in
  A5.1 for `view`, `write`, `edit`, and `multiedit`; classify every other
  workspace-capable tool (bash, custom, MCP, gateway-call, terminal/native,
  etc.) `PathScopeUnsupported` so Stage 3's fallback rules correctly keep
  using whole-root claims for them;
- wire `IntersectWritePathScopes`/`IntersectReadPathScopes`/
  `IntersectPathScopeCeilings` (added as pure helpers in Stage 3) into
  `cfgWithMergedPaths`, flipping its task-scope merge from union to
  intersection (A5);
- install `AgentTaskPathScope` on every worker/tool execution context in
  `coordinator_run.go`, `coordinator_task_run.go`, and
  `coordinator_declared_tool_runner.go` (A5 rule 10);
- add the symlink-race / TOCTOU regression (A-T14) proving the helper closes
  the validate-then-open window.

Only after this stage lands does a bounded task actually receive a narrow
path claim from Stage 3's A4 derivation rules; Stage 3's envelope/scheduler
machinery already handles the resulting concurrency correctly with no further
scheduler changes required here.

Files likely touched:

```text
internal/tools/tools.go
internal/tools/types.go
internal/tools/view.go
internal/tools/write.go
internal/tools/edit.go
internal/tools/multiedit.go
internal/tools/scoped_file_access_unix.go (new; os.OpenRoot-based helper)
internal/tools/scoped_file_access_unsupported.go (new; fail-closed capability
for GOOS=js/plan9 and any other platform lacking os.Root's confinement
guarantee)
internal/team/coordinator_task_run.go
internal/team/coordinator_run.go
internal/team/coordinator_declared_tool_runner.go
```

Acceptance:

- A-T04, A-T05, A-T08, A-T12 (configured/runtime intersection) from the
  Phase A test matrix;
- A-T14 (path authorization race), run under `-race`;
- A-T13 (backend eligibility);
- the performance measurement in §17 confirming disjoint bounded writers are
  no longer unnecessarily serialized.

## PR/Stage 5 — MCP dynamic gateway

Behavior:
- add `use_dynamic_tool`;
- proxy only eligible manager-owned MCP supplemental tools;
- preserve direct ToolSequence/protocol paths;
- route logical targets through the shared authorization path;
- add logical invocation receipt/audit/transcript/status projections;
- preserve frozen snapshot and cache semantics.

Files likely touched:

```text
internal/team/dynamic_tool_gateway.go (new; gateway handler + attempt catalog)
internal/team/services.go
internal/team/static_tool_resolution.go
internal/team/tool_policy_gate.go
internal/mcp/manager.go
internal/tools/*
internal/team/*receipt*
internal/team/task_transcript.go
internal/team/coordinator_taskcache.go
internal/team/cache_policy.go
internal/team/status.go
internal/team/projection_shadow.go
internal/audit/*
cmd/hufu/display.go
cmd/hufu/json_output.go
cmd/hufu/report.go
```

Exact files should be chosen after code search; do not create duplicate receipt infrastructure.

## PR/Stage 6 — Documentation and cleanup

Update active normative docs only after implementation behavior is stable.

Likely:

```text
docs/architecture/execution-runtime.md
docs/architecture/workset.md
docs/roadmap.md
AGENTS.md (only if a new invariant is truly repository-wide)
```

Do not rewrite archived implementation plans as if they were active authority.

---

# 16. Semantic-regression guardrails

The coding agent MUST explicitly check these before declaring completion.

## 16.1 Scheduler lifecycle

No change may cause:

```text
pending -> done
pending -> skipped
in_progress -> done
```

without canonical task lifecycle transitions.

The current HEAD specifically hardened TUI behavior so presentation cannot synthesize terminal task states. Preserve that ownership rule.

## 16.2 Task occurrence replay

A resumed/retried task must not:
- read live config to widen claims;
- read current MCP inventory to widen logical authorization;
- silently retarget execution;
- replace frozen workset binding with a newly generated one.

It may use live MCP inventory only to bind a frozen logical name to an
available handler after an exact descriptor-fingerprint match. New live names
are ignored; missing or changed frozen targets follow B4.1.

## 16.3 Result protocol

Preserve:

```text
RequiresResult
RequiresGroundedResult
submit_result
result repair
resume
```

semantics.

## 16.4 Static tool resolver

Do not create a second allow/deny implementation inside `use_dynamic_tool`.

Static resolver remains the decision source.

The extracted logical-target runtime gate remains the single enforcement path
for direct and proxied MCP calls. Gateway membership checks only narrow the
static result; they never replace it.

## 16.5 Workset migration

Do not reintroduce path-based workset identity or make deprecated TSV fan-out the canonical path.

## 16.6 Evidence

Never infer success from tool text or gateway response text.

---

# 17. Performance expectations

This work should improve scalability without sacrificing correctness.

Measure at minimum:

## Resource scheduling

Synthetic batch:
- N read tasks;
- N disjoint bounded writers;
- N overlapping writers.

Confirm:
- disjoint bounded local writers using only scope-enforced `view` /
  `write` / `edit` / `multiedit` surfaces are no longer unnecessarily
  serialized by scheduler claims;
- overlapping writers serialize;
- external full-snapshot providers remain serialized.

## Tool schema

Measure serialized provider-visible tool schema bytes with:

```text
0 MCP tools
10 MCP tools
50 MCP tools
100 MCP tools
```

Expected gateway-mode growth:

```text
eligible manager-owned MCP schema size ~= O(1)
```

for gateway-eligible ordinary MCP inventory. Direct built-ins, closed-sequence
targets, agent-specific command tools, and schema-ineligible MCP tools remain
outside this claim and must be reported separately in the measurement.

Logical target catalog may grow host-side; it must not all be injected into the provider request.

---

# 18. Documentation requirements

Document the distinction clearly:

```text
Resource claim       = scheduler conflict metadata
Allowed read path    = filesystem observation authorization
Allowed write path   = filesystem mutation authorization
Resource scope snapshot = durable effective claim/confinement contract
Workset touched path = durable bounded data/work metadata used for new scope creation
ExecutionWorld lease = snapshot/process isolation
```

Also document:

```text
provider-visible tool != logical authorized tool
live MCP inventory != frozen occurrence authorization
provider surface digest != logical task-cache digest
```

The gateway is representation, not authority.

---

# 19. Required validation

Per `AGENTS.md`, any code-changing stage MUST pass:

```bash
gofmt -w <changed-go-files>
go build ./cmd/hufu
go vet ./...
go test ./...
golangci-lint run
```

For scheduler/concurrency changes additionally run focused race coverage:

```bash
go test -race ./internal/team/...
go test -race ./internal/tools/...
go test -race ./internal/mcp/...
```

If the full race suite is impractical in the execution environment, run all
affected packages with `-race` and record exactly what was not run and why. Do
not report the task as fully verified when a required gate was skipped.

Run:

```bash
git diff --check
```

before completion.

---

# 20. Definition of Done

This specification is complete only when all of the following are true:

- [ ] legacy `ResourceClaim` behavior remains compatible;
- [ ] workspace path claims have deterministic hierarchical conflict semantics;
- [ ] bounded writer concurrency is enabled only together with enforced
      read/write confinement;
- [ ] initial batch envelope preflight completes before any task starts;
- [ ] unbounded workspace readers receive a whole-root read claim;
- [ ] unbounded writers fail safe to whole-workspace conflict scope;
- [ ] configured, runtime, and task read/write scopes are intersected rather
      than unioned;
- [ ] unsupported workspace-access tools/backends force whole-root fallback;
- [ ] path authorization race tests pass for every narrowly eligible reader
      and writer;
- [ ] `LocalExecutionWorld` still protects full-root snapshot delta attribution;
- [ ] retry/resume derives identical effective claims from durable occurrence state;
- [ ] `TaskResourceScopeSnapshot` survives every durable projection and legacy
      occurrences receive only whole-root scope;
- [ ] dynamic logical authorization and descriptor fingerprints survive every durable projection;
- [ ] retry/resume cannot gain a newly discovered MCP target;
- [ ] normal MCP inventory can be represented by one fixed `use_dynamic_tool` schema;
- [ ] agent-specific command tools and schema-ineligible MCP tools remain direct;
- [ ] static Hufu tool policy still decides logical authorization;
- [ ] gateway `search` / `inspect` cannot reveal unauthorized targets;
- [ ] gateway `call` rechecks exact target authorization;
- [ ] MCP `ToolAuthorizer` remains in the execution path;
- [ ] result-repair/result-only resume remains exactly `submit_result`;
- [ ] closed `ToolSequence` behavior is unchanged in v1;
- [ ] logical toolset changes still invalidate task-cache freshness correctly;
- [ ] task cache and in-flight de-duplication include logical toolset and
      resource-scope identity;
- [ ] receipts/evidence identify the logical target tool;
- [ ] transcript, audit, status, retry evidence, and report/JSON projections correlate gateway and logical calls;
- [ ] no new completion inference is introduced;
- [ ] `go build ./cmd/hufu` passes;
- [ ] `go vet ./...` passes;
- [ ] `go test ./...` passes;
- [ ] affected race tests pass;
- [ ] `golangci-lint run` passes;
- [ ] `git diff --check` passes.

---

# 21. Explicit stop conditions for the coding agent

Stop implementation and report the concrete incompatibility instead of forcing a patch if any of these are discovered:

1. current HEAD has replaced `ResourceClaim` or DAG resource ownership with a newer canonical abstraction;
2. `WorksetBinding.TouchedPaths` no longer has durable/frozen semantics;
3. no Hufu-local structured reader/writer combination can enforce
   task-specific allowed-read/write-path intersections before filesystem I/O
   (an individual unsupported tool is not a stop; classify it unsupported and
   use whole-root fallback);
4. MCP authorization, decision gates, or receipt hooks cannot be reused by a
   gateway without bypassing an existing policy hook;
5. `ResolvedWorkerTools.Names` has acquired a newer documented meaning incompatible with the proposed split;
6. task-cache identity relies on provider-visible schema identity in a way that would make logical-tool changes stale;
7. the durable task occurrence/event schema cannot carry the frozen dynamic
   authorization and resource-scope snapshots without violating a newer
   canonical abstraction;
8. a newer active ADR explicitly forbids either change.

In that case, produce:

```text
- current code path;
- conflicting invariant/ADR;
- minimal alternative;
- tests proving the conflict.
```

Do not silently weaken Hufu's current safety boundaries to satisfy this spec.

---

# 22. Rationale versus Reasonix

The relevant Reasonix ideas are:

```text
write-path-aware worker concurrency
fixed provider-visible capability gateway
host-owned authorization
stable schema surface
```

Hufu already has stronger or more specialized primitives in several adjacent areas:

```text
durable TaskOccurrenceProjection
ExecutionTarget / ExecutionRegistry
typed result protocol
evidence manifest / receipts
resource claims
workset bindings
repair controller
execution-world delta verification
```

Therefore the correct direction is **integration**, not architectural replacement.

The final Hufu model should be:

```text
                   durable TaskOccurrence
                           │
          ┌────────────────┴────────────────┐
          │                                 │
  effective resource claims         logical authorized tools
          │                                 │
  DAG scheduling / conflicts        StaticToolResolution
          │                                 │
  enforced write confinement        provider-surface projection
          │                           ├─ direct tools
          │                           └─ use_dynamic_tool
          │                                 │
          └──────────────┬──────────────────┘
                         ▼
                  authorized attempt
                         │
                  ExecutionBackend
                         │
             receipt / verify / evidence
                         │
                    acceptance
```

This preserves Hufu's core architectural rule:

> Non-deterministic agents may propose and execute work only inside deterministic, durable, runtime-owned boundaries.
