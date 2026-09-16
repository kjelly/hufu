# Decision Runtime Profile Generalization — Implementation Plan

**Status:** completed 2026-09-16; archived implementation record
**Authority:** [`docs/architecture/decision-runtime.md`](../../architecture/decision-runtime.md)
**Repository:** `github.com/kjelly/hufu`
**Scope:** generalize the existing Decision Runtime without changing its stage
semantics, event ordering, authorization, or recovery behavior.

**Implementation commits:** Phase 0 `9bc360b`; Phase 1 `b45565d`; Phase 2
`ab0a16e`; Phase 3 `7129615`; Phase 4 `af41bfe`; Phase 5 `fbd0f98`; final
legacy recovery acceptance `4242666`. Phase 6 was explicitly optional and was
not needed for the runtime acceptance criteria, so no new CLI surface was added.

This document is a change plan, not a replacement architecture specification.
If this document conflicts with `docs/architecture/decision-runtime.md`, the
architecture document wins and this plan must be corrected before code is
written.

## 1. Decision

Make decision profiles composable, versioned, and materialized at admission,
while keeping the current package boundary:

Here, composable means selecting an existing complete `DecisionPolicy` made
of the existing policy primitives. V1 adds neither inheritance nor overlays.

```text
internal/agent
  declarative policy types
  profile references/specifications
  built-in profile catalog
  policy canonicalization and digest

internal/team
  team adapter and precedence resolution
  admission and durable envelope
  DecisionEngine and existing stage state machine
  execution-plan projection

cmd/hufu
  CLI override and presentation only
```

Do **not** create `internal/decision`, `internal/policy`, or
`internal/routing` in this change. `internal/team` owns `TaskDef`,
`EventJournal`, `ArtifactStore`, and `ArtifactRef`; extracting runtime types
without first moving those dependencies would create an import cycle or a
large unrelated refactor. This is the package decision already recorded in
the active architecture document.

The semantic rule is:

```text
same materialized policy
  + same immutable request
  + same runtime services
  = same decision-stage semantics
```

Profile names are identifiers and telemetry labels only. They must never select
special execution branches. `off` is the sole reserved name and only disables
structured decision formation; it never disables safety, authorization,
durability, verification, or recovery.

## 2. Verified current baseline

The following already exists and must be reused:

- [`internal/agent/decision_config.go`](../../../internal/agent/decision_config.go)
  contains `DecisionConfig`, `DecisionPolicy`, all policy subtypes, and strict
  validation.
- [`internal/team/decision_config.go`](../../../internal/team/decision_config.go)
  resolves request/task/team profile precedence and rejects unknown team
  profiles.
- [`internal/team/decision_engine.go`](../../../internal/team/decision_engine.go)
  exposes `NewDecisionEngine(DecisionServices)` and `DecisionEngine.Run`/
  `Resume`; it does not require a `TeamConfig`.
- [`internal/team/decision_admission.go`](../../../internal/team/decision_admission.go)
  durably freezes the effective profile, policy, task occurrence, and input
  digest before execution.
- [`internal/team/decision_envelope.go`](../../../internal/team/decision_envelope.go)
  persists an immutable policy/request snapshot and resume loads that snapshot
  instead of current team configuration.
- [`internal/team/decision_engine_stages.go`](../../../internal/team/decision_engine_stages.go)
  and the other `decision_*.go` files implement the durable stage procedure.
- [`internal/team/decision_canonical.go`](../../../internal/team/decision_canonical.go),
  `decision_numeric.go`, evidence sealing, and event projection already provide
  canonical evidence and deterministic numeric behavior.
- `--decision-profile` already exists in [`cmd/hufu/root.go`](../../../cmd/hufu/root.go)
  and is passed as a run-scoped override.
- Existing inline team profiles remain supported; at the start of this plan,
  the strategic team defined `light`, `standard`, and `high-stakes` inline in
  [`.agent-teams/strategic-decision/team.yaml`](../../../.agent-teams/strategic-decision/team.yaml).

Baseline validation before the refactor:

```text
go test ./internal/agent ./internal/team
```

The baseline must remain green after every phase. Code changes additionally
require `golangci-lint run`.

Readiness review on 2026-09-16 ran the command above successfully. This is
evidence for the current baseline, not evidence that the proposed code exists.
Phase 0 must record the implementation checkout's commit and rerun the gates.
The architecture's §8.1 records the approved target contract separately from
implemented behavior.

One existing boundary matters: `decision_budget.go` supports explicit policy
degradation for direct engine calls, but `validateDecisionAdmissionEnvelope`
requires exact admission/envelope policy equality for coordinator occurrences.
A policy-changing degradation in that path is rejected today. All three
strategic profiles use `budget-degradation: forbidden`. This refactor preserves
both behaviors; §9.3 defines digest handling without expanding that boundary.

## 3. Goals and non-goals

### Goals

1. Resolve a profile into a complete immutable policy before the first
   decision stage runs.
2. Support versioned built-ins such as `builtin/standard@v1`.
3. Preserve custom inline profiles without silently converting them to built-ins.
4. Persist requested name, canonical reference, origin, applicable version,
   policy digest, and full policy snapshot for new admissions.
5. Resume exclusively from the durable snapshot.
6. Make policy-to-stage behavior inspectable through a derived execution plan.
7. Prove name independence and use of the runtime without the strategic team.

### Non-goals

- moving `DecisionEngine` or artifact/event types into a new top-level package;
- replacing the existing DecisionEngine state machine with a workflow DSL;
- dynamic primitive/plugin loading;
- profile inheritance or partial policy patches;
- changing stage order, aggregation arithmetic, gate behavior, tool
  authorization, recovery, or event semantics;
- making `strategic` or `debiasing` special runtime modes;
- automatic global profile aliases in `hufu.yaml` during the first implementation;
- new worker/tool authorization granted by a profile.

## 4. Profile data model

Add the following pure configuration-side types under `internal/agent`.
They must not import `internal/team`.

```go
type DecisionProfileRef struct {
    Name string
}

type DecisionProfileSpec struct {
    Preset *DecisionProfileRef `yaml:"preset,omitempty"`
    Policy *DecisionPolicy     `yaml:"policy,omitempty"`
}

type DecisionProfileMetadata struct {
    Ref         string
    Origin      string
    Version     string
    Description string
    Stability   string
}

type MaterializedDecisionProfile struct {
    RequestedName string
    Ref           string
    Origin        string
    Version       string
    Policy        DecisionPolicy
    PolicyDigest  string
}

type DecisionProfileCatalog interface {
    Resolve(ref DecisionProfileRef) (DecisionPolicy, DecisionProfileMetadata, error)
}
```

Metadata identity is fixed for V1:

- built-in `builtin/standard@v1` has `Origin: "builtin"` and `Version: "v1"`;
- a team-local inline policy has `Origin: "team-inline"` and an empty version;
- a request-local inline policy has `Origin: "request-inline"` and an empty
  version;
- a v1 durable snapshot missing metadata is represented on a new envelope as
  `Origin: "legacy-inline"`, with empty version and reference;
- `Ref` is the complete canonical reference, for example
  `builtin/standard@v1`, or empty for an inline policy.

`RequestedName` records the selected local name or direct reference. `Ref`
records the catalog identity even when selected through a local alias. A CLI
override selecting a team-local policy remains `team-inline`; selection source
(`request`, `task`, `team`, `default`) is distinct from policy origin.
`request-inline` is only for trusted direct engine callers supplying a complete
policy; this change adds no inline CLI flag or LLM-authored policy input.

Validation rules:

- `DecisionProfileSpec` must contain exactly one of `Preset` or `Policy`.
- A built-in reference must be `builtin/<name>@<version>`; missing or unknown
  versions fail closed.
- `off` is not a catalog entry and cannot be declared in `profiles`.
- No `extends` or field overlay is accepted in V1.
- A custom policy is validated using the existing `DecisionPolicy.Validate`.
- `DecisionProfileRef` encodes/decodes a YAML string scalar, not a `{name: ...}`
  mapping. Reject null, empty, non-string, and unversioned preset values.
- Catalog results and materialized policies are deep copies, including nested
  pointers and slices. Mutating one caller's policy must not mutate the catalog,
  another result, either compatibility map, or a stored snapshot.

### Internal compatibility representation

Do not immediately change every caller from `map[string]DecisionPolicy`.
During migration, add exactly one field to the existing `DecisionConfig` while
leaving all current fields, tags, and policy maps intact:

```go
ProfileSpecs map[string]DecisionProfileSpec `yaml:"-"`
```

The strict decoder populates both maps: `Profiles` receives the effective policy
for compatibility with existing callers, while `ProfileSpecs` retains whether
the source was a preset or an inline policy. A programmatic config that only
sets `Profiles[name]` is interpreted as a team-local inline policy. `HasProfile`
recognizes names from either map, but does not treat a name as built-in merely
because it is called `standard`.

`ProfileSpecs` is the authority whenever an entry exists. `Profiles` is a
compatibility projection, never an independently mutable override:

- At config validation/materialization, resolve each spec and normalize its
  policy. If the same key exists in `Profiles`, compare canonical policy bytes;
  reject a semantic mismatch instead of picking a winner silently.
- A `Profiles`-only entry is adapted to an inline spec. A specs-only entry is
  valid and receives a compatibility projection in the returned config copy.
- Use one config materialization helper in `internal/agent`, accepting the
  catalog and returning a validated deep copy with both maps. `Validate()` can
  call it without retaining the copy; parser/resolver callers retain it.
  Avoid recursive `Validate` calls inside that helper: validate individual
  policies, specs, request-contract and routing hints directly. YAML adapters
  and existing public compatibility helpers use the compiled catalog; the
  injected catalog is used consistently throughout a resolver invocation.
- Validation returns errors; boolean membership helpers cannot be used as a
  substitute for resolving/validating a profile. Validate defaults, all declared
  profiles, and task references, including unused malformed specs.
- Existing `ResolveDecisionProfile` and `DecisionPolicyFor` callers must share
  the new resolution implementation. No dispatch path may continue to use a
  direct map lookup that excludes built-ins or ignores spec conflicts.
- Configuration updates rebuild and validate the projection before publication;
  runtime admissions take independent snapshots. Do not mutate an admitted policy.

## 5. Built-in catalog

Create the catalog in `internal/agent` using exactly these files:

```text
internal/agent/decision_profiles.go
internal/agent/decision_profiles_test.go
```

The catalog is a compiled, deterministic map. Embedded YAML is optional; plain
Go declarations are acceptable if they are easier to validate and keep small.

First catalog release:

```text
builtin/light@v1
builtin/standard@v1
builtin/high-stakes@v1
```

These three must be exact policy snapshots of the corresponding current
profiles in `.agent-teams/strategic-decision/team.yaml`. Capture their policy
digests before changing the team file. `builtin/strategic@v1` and
`builtin/debiasing@v1` are not included until their complete policy fields and
acceptance behavior are explicitly defined; they are compositions, not
runtime modes, and must not be invented from names alone.

Catalog requirements:

- `Resolve` accepts only an exact, versioned reference.
- `builtin/standard` never means “latest” inside durable runtime code.
- catalog entries are immutable within a version;
- descriptions and stability are metadata only;
- the catalog contains no worker, tool, credential, or authorization data.
- Adding/changing fields or canonical encoding must not change the frozen v1
  catalog digests. Such a change requires an explicit format/version migration.

## 6. YAML and precedence compatibility

The current strict team manifest decoder is in
[`internal/team/team_manifest.go`](../../../internal/team/team_manifest.go) and
[`internal/team/parse.go`](../../../internal/team/parse.go). Keep strict unknown
field behavior for both legacy and new profile shapes.

Support both forms:

```yaml
decision:
  profiles:
    legacy-custom:
      independent-judgments: 2
      aggregation:
        method: mean-score

    standard:
      preset: builtin/standard@v1

    custom:
      policy:
        independent-judgments: 2
        aggregation:
          method: mean-score
```

The profile decoder must classify an entry as follows:

1. If it contains `preset` or `policy`, decode the tagged form and reject any
   unknown key.
2. Otherwise decode it as the legacy inline `DecisionPolicy` and record it as
   a custom policy.
3. Reject entries containing both `preset` and `policy`.
4. Do not reinterpret an inline profile named `standard` as
   `builtin/standard@v1`.

Preserve `KnownFields(true)` recursively, including inside a custom
`UnmarshalYAML` implementation; `yaml.Node.Decode` alone does not inherit the
outer decoder's strictness. Reject duplicate keys, null entries, non-mapping
profile entries, mixed legacy/tagged keys, and unknown nested policy fields.
Apply the same decoder to legacy flat, `advanced:` compatibility, and
`hufu.io/v1alpha1` manifests.

### 6.1 Serialization and migration

Add a paired `DecisionConfig.MarshalYAML` (or an equivalent shared wire adapter)
that serializes validated specs as the `profiles` authoring map. A preset stays
`preset: builtin/<name>@<version>`; an inline entry may be written in the tagged
`policy:` form even if originally legacy inline. `Profiles`-only programmatic
configs serialize as inline policies. Reject conflicting maps before encoding.
`yaml:"-"` on `ProfileSpecs` does not permit silently serializing its expanded
compatibility map in place of the authored preset.

`MigrateTeamManifestToV1Alpha1` and ordinary marshal/load round trips must
preserve requested local name, canonical reference, origin/version, and policy
digest. Formatting and inline wrapper spelling may change. Retain existing
template and `advanced:` migration behavior; never render template variables
as a side effect of migration. Test both manifest schemas and already-versioned
re-serialization through the actual migrator.

### 6.2 Selection and validation

Resolution precedence remains the current chain:

```text
--decision-profile / request override
    > TaskDef.DecisionProfile
    > decision.default-profile
    > off
```

At each non-empty layer, resolve in this order:

- `off` is accepted directly;
- an exact built-in reference goes to the catalog;
- a team-local name goes to `ProfileSpecs`;
- a bare local name that is not declared fails closed;
- no layer selected means `off`.

Reserve the `builtin/` prefix for catalog references; reject local profile keys
using it. Malformed or unknown references under that prefix fail closed without
falling back to a local name or lower precedence layer. Local names otherwise
retain current case-sensitive behavior and selection keeps existing whitespace
trimming. `off` is legal in every selection layer but not as a declared profile.

Update `DecisionConfig.Validate`, `HasProfile`, task validation, run override
preflight, and resolver consumers consistently. Exact built-ins require no local
alias. Selecting one still requires all existing request-contract, runner,
evidence, authorization, and budget checks; a catalog entry supplies none of
those services. Unknown references fail with `decision_profile_unknown` before
dispatch. The normative extension is architecture §8.1 and §9.

The first implementation does not add a global `runtime.decision.aliases`
configuration. That can be a later adapter once its merge and precedence
semantics are specified in `internal/config`; it must not be smuggled into the
team parser as an undocumented field.

## 7. Materialization and admission

Add the resolver in `internal/team/decision_config.go`, backed by the pure
catalog in `internal/agent`:

```go
func ResolveMaterializedDecisionProfile(
    cfg DecisionConfig,
    requestOverride string,
    task any,
    catalog agent.DecisionProfileCatalog,
) (agent.MaterializedDecisionProfile, DecisionProfileResolution, error)
```

The resolver must:

1. apply the precedence chain;
2. resolve the selected local spec or exact built-in reference;
3. apply existing policy defaults and `DecisionPolicy.Validate`;
4. canonicalize the complete materialized policy;
5. calculate `PolicyDigest` as `sha256:<lowercase-hex>`;
6. return the requested name separately from origin/version;
7. return an error before any model/tool dispatch on failure.

After materialization, execution receives only the materialized policy and
metadata. No stage may call the catalog or inspect current team configuration.
This restriction concerns profile resolution; existing service/authorization
adapters remain intact. `off` returns its resolution plus an empty materialized
policy/identity, bypasses policy validation/digesting, and still writes the
existing off admission marker.

The existing `DecisionAdmission` remains the task-occurrence admission
boundary. Extend it with:

```go
ProfileOrigin  string `json:"profile_origin,omitempty"`
ProfileVersion string `json:"profile_version,omitempty"`
ProfileRef     string `json:"profile_ref,omitempty"`
PolicyDigest   string `json:"policy_digest,omitempty"`
```

The existing `Policy` field remains the authoritative full snapshot. Admission
validation must recompute the digest from `Policy` and reject a mismatch. The
existing `Profile` field remains the requested/local display name; it is not
the durable source of policy semantics. Add the same four metadata fields to
`DecisionRequest`. Thread them through `decision_dispatch.go` from the loaded
admission, through request cloning, to `newDecisionRunEnvelope`; adding fields
only to persistence structs is insufficient. The envelope digest binds the
policy actually stored there, subject to §9.3.

For trusted direct `DecisionEngine.Run` calls without an admission, absent
metadata means `request-inline`: normalize the supplied policy, compute its
digest, and set empty version/ref before dispatch. If any identity metadata is
supplied, require a complete valid combination from §9.1 and verify its digest;
do not silently repair partially supplied or mismatched metadata. Catalog
callers materialize first and provide all identity fields. The engine never
consults a catalog. Resume first selects the durable envelope and retains the
existing request-identity checks, before considering fresh-request defaults.

## 8. Canonical policy digest

Add the pure helpers under `internal/agent/decision_policy_digest.go`:

```go
func CanonicalDecisionPolicy(p DecisionPolicy) ([]byte, error)
func DecisionPolicyDigest(p DecisionPolicy) (string, error)
func NormalizeDecisionPolicy(p DecisionPolicy) (DecisionPolicy, error)
```

Canonicalization rules:

- validate first; reject NaN and infinity;
- normalize semantic defaults before encoding;
- use one fixed JSON representation, never YAML text;
- preserve order where a slice is semantically ordered;
- sort map keys deterministically;
- encode `-0` consistently as `0`;
- hash the exact canonical bytes with SHA-256.

Freeze a private V1 canonical JSON DTO (including nested DTOs), with explicit
field names/order matching the current `DecisionPolicy` JSON representation at
Phase 0 and the current `max_tokens` tag. Do not hash the evolving runtime
struct directly. Use compact `encoding/json` output, default HTML escaping,
no trailing newline, and normalize empty slices to nil consistently. Include
every policy field, including optional routing pins and discipline fields;
preserve slice order in V1. New policy fields require an explicit decision on
canonical format compatibility, never silent omission from the digest.
Schema-v2 records use this V1 digest encoding; readers must retain that encoder
when a future schema introduces another one.

`NormalizeDecisionPolicy` is the only place that materialization fills
semantic defaults: `min-independent-judgments`, `max-rounds`,
`context-isolation`, `aggregation.method`, `finalization.mode`,
`budget-degradation`, and the effective option cap. `CanonicalDecisionPolicy`
operates on that normalized copy;
the resolver stores the same normalized copy in `MaterializedDecisionProfile`
and in new admission/envelope snapshots. This list defines V1's default
equivalence; do not claim every behaviorally equivalent policy has one digest.
Other omitted/explicit defaults remain distinct encodings. Normalization is
idempotent, validates its result, and never mutates the input. Legacy raw
snapshots use the compatibility exception in §9.2.

Do not use reflection-based merge logic or an LLM-generated digest. Add golden
tests for repeated encoding, map insertion order, equivalent default forms,
invalid floats, and changed policy fields.

## 9. Durable envelope and resume

Change `DecisionRunEnvelopeSchemaVersion` from `1` to `2`, and extend
[`DecisionRunEnvelope`](../../../internal/team/decision_envelope.go) with:

```go
ProfileOrigin  string `json:"profile_origin,omitempty"`
ProfileVersion string `json:"profile_version,omitempty"`
ProfileRef     string `json:"profile_ref,omitempty"`
PolicyDigest   string `json:"policy_digest,omitempty"`
```

### 9.1 Metadata validation

For enabled schema-v2 admissions and envelopes, origin and digest are non-empty;
version/ref are conditional, not universally required:

| Origin | Version | ProfileRef | PolicyDigest |
|---|---|---|---|
| `builtin` | exact reference suffix, initially `v1` | complete versioned reference | required |
| `team-inline` | empty | empty | required |
| `request-inline` | empty | empty | required |
| `legacy-inline` | empty | empty | required for a new envelope derived from v1 admission |

`legacy-inline` is not accepted for newly authored admissions or fresh direct
requests. `request-inline` is not a team-config origin. Reject unknown origins,
wrong version/ref combinations, and a digest other than `sha256:` plus 64
lowercase hexadecimal digits matching the policy. Durable validation checks
reference syntax and internal consistency, never current catalog membership.
Origin identifies how the policy was obtained; it grants no authority.

`Policy` remains the full snapshot. Envelope metadata must agree with its
nested request and, when present, the occurrence admission under §9.2/§9.3.
`off` admissions carry no policy or profile metadata and create no envelope.
Old records retain their prior required-field rules.

### 9.2 Version compatibility and crash boundaries

Backward compatibility:

- `loadDecisionRunEnvelope` accepts schema versions 1 and 2;
- schema-version-1 validation keeps its existing required fields and permits
  missing profile metadata;
- newly written envelopes use schema version 2;
- treat absent metadata in a valid old envelope as legacy for presentation only;
  do not mutate its decoded policy or rewrite its bytes;
- do not retroactively calculate a new origin from the current catalog;
- write the new schema version only for newly created envelopes;
- reject a present-but-invalid digest or metadata identity mismatch.

Resume rules:

```text
event anchor -> envelope artifact -> envelope.Policy
```

Resume must never perform:

```text
envelope.Profile -> current team config/catalog -> new policy
```

Change `DecisionAdmissionSchemaVersion` from `1` to `2` using the same rule:
read versions 1 and 2, write version 2, and require metadata only for new
enabled admissions. Add tests for policy snapshot reuse, catalog mutation, team
profile mutation, metadata preservation, tampered policy, and digest mismatch.

Compatibility behavior is defined for each durable boundary:

| Durable state at restart | Required behavior |
|---|---|
| v1 admission, no envelope | Load its raw policy; create a v2 envelope using that exact policy, with legacy identity when metadata is absent (see below), and digest computed from a normalized copy of the raw policy. Never rewrite the admission. |
| v1 admission + v1 envelope | Preserve both snapshots and the existing exact identity/policy checks. Missing metadata is accepted; no new admission/envelope is written. |
| v1 admission + bridge v2 envelope | Verify the bridge origin, raw policy equality, digest, occurrence, task-input digest, and request-contract identity using only the durable records. |
| v2 admission, no envelope | Populate request metadata/policy from admission and create a matching v2 envelope. |
| v2 admission + v2 envelope | Require full metadata equality as well as all existing occurrence/policy checks. |
| v2 admission + v1 envelope | Reject as an unsupported downgrade. |
| Envelope without admission | Direct engine runs remain valid without an occurrence admission. For coordinator recovery, preserve only the existing narrowly validated legacy-envelope compatibility path; do not fabricate an admission. |

The bridge is the sole new-envelope exception to storing normalized policy:
retain the v1 raw policy so `reflect.DeepEqual` and stage inputs do not change.
Digest computation normalizes only a copy. If optional metadata exists in v1,
validate every present field and any present digest; preserve a complete valid
identity when bridging. Only fully absent metadata uses `legacy-inline`;
partially supplied identity fails closed when constructing a new v2 envelope.
Define absent as all four identity fields empty. For v1 reads, validate supplied
field syntax and any relationships whose operands are present, without requiring
missing fields; never infer missing origin/ref/version from the display name.
Both sides supplying the same metadata field must agree. Envelope/nested-request
metadata must agree wherever present; v2 requires exact equality of all fields.

Update `validateDecisionAdmissionEnvelope`, `validateDecisionRunRequestIdentity`,
and request/envelope validation together. A request with supplied metadata that
conflicts with an existing envelope fails before stages run; a resume request
that supplies identity only uses the envelope's snapshot. Test crashes before
the first decision event, before envelope anchoring, and after anchoring, with
the current team/catalog removed or changed. Existing unsafe in-flight stage
recovery remains fail-closed; a schema bridge does not authorize rerunning it.

### 9.3 Budget degradation and snapshot identity

The admission digest binds the configured occurrence policy. The envelope and
its nested request digest bind the final policy returned by `admitBudget`.
For direct engine runs without an occurrence admission, recompute the envelope
digest after any allowed degradation and before persistence; retain origin,
reference, and version as source provenance. A built-in origin does not assert
that a degraded policy still equals the catalog entry. Preserve existing
`decision_budget_degraded` events and ordering; do not add an authoritative
second policy or rerun budget admission after an envelope is anchored.

For coordinator occurrences with an admission, preserve the existing exact
policy-equality gate: policy-changing degradation still fails at envelope
validation and must not reach JUDGE dispatch. Do not change equality to
digest-only comparison or treat any degradation event as permission to bypass
the gate. Supporting successful coordinator degradation requires a separate
admission-transition/recovery contract and is outside this refactor. This is
a documented existing limitation, not a new failure introduced by metadata.
Characterize it in Phase 0 alongside the successful direct-engine degradation
case, then verify both remain unchanged with digest validation enabled.

## 10. Execution plan projection

Add a derived, non-authoritative plan in the new file
`internal/team/decision_execution_plan.go`:

```go
type DecisionExecutionPlan struct {
    SchemaVersion   int
    Proposal        bool
    Reference       bool
    JudgeCount      int
    Aggregation     string
    Challenge       bool
    ChallengeCount  int
    Revision        bool
    Premortem       bool
    Forecast        bool
    Finalization    string
}

func CompileDecisionExecutionPlan(p DecisionPolicy) (DecisionExecutionPlan, error)
```

The compiler must call existing effective-policy helpers and validate the
result. It may be used for preflight, explain output, and telemetry. The
engine continues to use its existing explicit stage flow; the plan is not a
second configuration language and is not independently persisted as authority
in V1.

Plan schema version is 1. It describes policy eligibility/requirements, not
guaranteed dispatches. Define fields as follows: `Proposal` is proposal enabled;
`Reference` is outside-view required AND reference-evidence enabled;
`JudgeCount` is independent judgments; `Aggregation` and `Finalization` use
their effective helpers; `Challenge` is enabled AND count > 0;
`ChallengeCount` is count when eligible, otherwise zero; `Revision` is revision
enabled AND effective max-rounds >= 2 AND challenge eligible; `Premortem` is
premortem enabled; `Forecast` is forecast required. Actual proposal/reference
execution also depends on supplied options/base rates, challenge on aggregate
dispersion, and revision on challenge results. Presentation must label these
conditions rather than claim the compiler predicted a run. Admission plans use
the configured policy; an envelope plan derives from its effective policy.

Add a semantic comparison test proving that two distinct profile names with
identical materialized policies produce identical plans and stage behavior.
Compare runner inputs, dispatch counts/order, gate outcomes and final choice.
Exclude deliberately different display names, occurrence IDs, timestamps,
CAS identifiers and event hash-chain bytes from trace equality; metadata and
schema changes legitimately change those bytes, but not event type ordering.

## 11. Generic invocation proof

No new public `decision.Runtime` package is required for this change. The
existing `NewDecisionEngine(DecisionServices)` is already a TeamConfig-free
runtime entry point. Make this explicit with an integration test that:

1. constructs `DecisionServices` with in-memory journal/store and deterministic
   runners;
2. resolves `builtin/standard@v1` through the catalog;
3. calls `NewDecisionEngine(services).Run(DecisionRequest{...})`;
4. interrupts after envelope anchoring but before finalization, creates a fresh
   engine, and resumes the unfinished decision from the same store/journal;
5. never loads `.agent-teams/strategic-decision` or a `TeamConfig`.

The test belongs under `internal/team`, not under the strategic team fixture.
Supply deterministic runners for every enabled stage, valid evidence and
contract inputs, and a budget sufficient for the unchanged standard profile.
Assert already-completed stage work is reused and the final result matches an
uninterrupted run. A second `Resume` of an already-finalized decision alone is
not sufficient proof of recovery. Include a metadata-free direct inline call
to prove source compatibility for existing trusted engine users.
The existing `evals/decision-runtime` suite remains a team/coordinator
compatibility suite and is not claimed as proof of direct runtime invocation.

A future API-facing runtime facade may be added only after concrete contracts
for journal/store lookup, authorization context, workspace scope, and resume
service discovery are specified.

## 12. Authorization and routing

The profile/catalog layer may describe required or preferred capabilities, role
counts, and diversity constraints only through existing policy fields. It must
never provide:

- allowed worker names;
- tool allowlists;
- credentials;
- provider access;
- workspace or side-effect authority.

`internal/team` continues to resolve candidates from the already-authorized
worker set. Routing can narrow that set but cannot expand it. Existing
capability-routed REFERENCE/JUDGE/CHALLENGE/REVISE behavior must be unchanged.

Add a regression test that a preset cannot authorize an otherwise unauthorized
worker or tool.

## 13. Implementation phases

### Phase 0 — Freeze characterization

Before code changes:

- record the effective `light`, `standard`, and `high-stakes` policies from
  the strategic team;
- record their current policy behavior and resume tests;
- confirm the existing decision matrix and routing tests pass;
- record the current envelope schema and event order.
- freeze v1 admission/envelope fixture bytes, including omitted defaults;
- characterize successful direct-engine explicit degradation and coordinator
  rejection when degradation changes an admitted policy, as specified in §9.3.

Gate: no behavior change and baseline tests green.

### Phase 1 — Pure profile/catalog layer

Files:

```text
internal/agent/decision_profiles.go
internal/agent/decision_profiles_test.go
internal/agent/decision_policy_digest.go
internal/agent/decision_policy_digest_test.go
```

Implement references, tagged specs, metadata, catalog, canonical digest, and
the three built-ins. Do not change engine dispatch.

Gate:

- built-ins resolve only with exact versions;
- unknown versions fail closed;
- digests are golden and deterministic;
- `DecisionPolicy.Validate` remains the single policy validator.

### Phase 2 — Strict compatibility parser and resolver

Files:

```text
internal/agent/decision_config.go
internal/agent/decision_profiles_yaml.go
internal/agent/decision_profiles_yaml_test.go
internal/team/parse.go
internal/team/team_manifest.go
internal/team/team_manifest_migrate.go
internal/team/team_manifest_migrate_test.go
internal/team/decision_config.go
internal/team/decision_profile_test.go
```

Add tagged `preset`/`policy` parsing while retaining legacy inline profiles.
Update `HasProfile`, precedence resolution, and task validation to use the
profile spec metadata. Preserve `TaskDef.DecisionProfile` as configuration-only
(`json:"-"`).

Implement both decode and encode adapters, map consistency validation, direct
built-in reference preflight, and preset-preserving manifest migration. Trace
all resolver consumers, including CLI override checks in `cmd/hufu`, and add
their files to the change set where needed. File lists identify ownership
anchors, not an exhaustive restriction on necessary integration changes.

Gate: all old team fixtures retain their effective policies; unknown fields and ambiguous
profile forms fail closed; round trips retain preset provenance; conflicting
maps fail; all selection layers recognize exact built-ins consistently.

### Phase 3 — Materialized admission and envelope metadata

Files:

```text
internal/team/decision_admission.go
internal/team/decision_envelope.go
internal/team/decision_engine.go
internal/team/decision_engine_envelope.go
internal/team/decision_dispatch.go
internal/team/decision_budget.go
internal/team/decision_admission_test.go
internal/team/decision_dispatch_test.go
internal/team/decision_budget_test.go
internal/team/decision_stage3_test.go
```

Persist origin/version/ref/digest with new admissions, validate them, and make
the engine receive the materialized snapshot. Keep schema-version-1 read
support and implement every mixed-version path in §9.2. Integrate fresh direct
request materialization, deep cloning, and effective envelope digest handling.

Gate: resume ignores catalog/team mutations and refuses tampered snapshots;
v1 admissions can reach v2 envelopes without rewriting durable data; direct
degradation succeeds with a matching digest while the coordinator's existing
policy-equality rejection remains intact.

### Phase 4 — Derived execution plan and invariants

Files:

```text
internal/team/decision_execution_plan.go
internal/team/decision_execution_plan_test.go
internal/team/decision_architecture_test.go
```

Compile the plan at admission/preflight and add:

- profile-name independence test;
- no production branch on `light`, `standard`, `high-stakes`,
  `strategic`, or `debiasing`;
- preset authorization non-escalation test.

Gate: existing event order, stage sequence, and routing traces are unchanged.

### Phase 5 — Generic runtime proof and team migration

Add the TeamConfig-free direct engine test described in §11. Only after it is
green, change strategic-decision profiles to:

```yaml
decision:
  default-profile: standard
  profiles:
    light:
      preset: builtin/light@v1
    standard:
      preset: builtin/standard@v1
    high-stakes:
      preset: builtin/high-stakes@v1
```

Update the strategic team README to describe it as a consumer of Decision
Runtime. Keep its workers, capability registry, routing policy, authorization,
and request contract in the team.

Gate: old and migrated team configurations produce equivalent materialized
policies and decision traces.

### Phase 6 — Optional CLI presentation

Only after runtime behavior is complete, consider:

```text
hufu decision profiles
hufu decision profile show builtin/standard@v1
hufu decision profile resolve <name>
```

CLI commands may display catalog metadata and compiled plans but must not
reimplement policy semantics. These commands are not a blocker for the core
refactor.

## 14. Required test matrix

At minimum add tests covering:

```text
profile spec requires exactly one preset or policy
legacy inline profile remains custom
custom profile named standard is not a builtin
builtin profile exact-version resolution
unknown builtin version fails closed
all selection layers accept exact built-ins without a local alias
reserved builtin namespace and malformed references cannot fall back
strict nested YAML decoding, duplicate/null/mixed-shape rejection
specs-only and legacy-map-only programmatic configurations
conflicting compatibility maps fail validation and serialization
preset provenance survives both schema round trips and actual manifest migration
catalog/materialized/request copies do not share mutable policy data
stable policy digest
frozen canonical DTO covers all current policy fields
normalization is idempotent and non-mutating
equivalent policies produce equivalent execution plans
profile name does not alter stage semantics
off remains inert for structured decision formation
admission stores origin/applicable-version/ref/digest/policy
inline enabled admissions/envelopes accept an empty version/ref
invalid or partial metadata fails closed
metadata-free direct calls receive request-inline identity
v1 admission without envelope creates a v2 bridge using the raw snapshot
v1 admission with v1/v2 envelope resumes without rewriting old records
v2 admission with v1 envelope is rejected
valid legacy envelope without admission keeps its existing compatibility path
present invalid v1 metadata/digest is rejected
direct budget degradation recomputes the effective envelope digest
coordinator policy-changing degradation retains its existing rejection
resume uses envelope policy snapshot
resume ignores catalog mutation
resume ignores team profile mutation
resume rejects digest mismatch
direct engine invocation works without TeamConfig
preset cannot grant worker/tool authorization
strategic team preset migration preserves behavior
```

Existing decision, resume, routing, event-integrity, and fail-closed tests
remain mandatory; new tests supplement rather than replace them.

## 15. Definition of done

- [x] Active architecture remains the authority and this plan no longer
      conflicts with its profile selection or snapshot contracts.
- [x] `DecisionPolicy` remains canonical execution configuration.
- [x] Versioned built-ins `light@v1`, `standard@v1`, and `high-stakes@v1`
      resolve deterministically.
- [x] Legacy inline profiles remain backward compatible.
- [x] Specs own configuration; conflicting compatibility projections fail closed.
- [x] YAML and manifest migration preserve preset identity.
- [x] Materialization occurs before model/tool dispatch.
- [x] New admission/envelope records contain origin, applicable version/ref,
      digest, and full policy snapshot under §9.1.
- [x] Every §9.2 version/crash boundary has a regression test.
- [x] Effective envelope digests handle direct budget degradation without
      weakening the coordinator admission gate.
- [x] Resume never re-resolves a profile from live configuration.
- [x] Execution plans are derived only and do not create a second state machine.
- [x] No stage branches on methodology/profile names.
- [x] Direct `DecisionEngine` invocation works without `TeamConfig` or the
      strategic-decision team.
- [x] Presets cannot expand authorization or tool access.
- [x] Strategic team references built-ins after compatibility tests pass.
- [x] `go test ./...` and `golangci-lint run` pass for the implementation.

## 16. Stop conditions

Stop and revise this plan instead of adding a workaround if any of the
following occurs:

- a built-in cannot be represented by the existing `DecisionPolicy` fields;
- a requested change requires profile-name branching;
- resume would need current catalog/team configuration;
- the new catalog needs `TaskDef`, `ArtifactRef`, or event-store internals;
- a preset would need to grant authorization;
- compatibility parsing cannot preserve strict unknown-field rejection.

In those cases, document the missing primitive or contract in
`docs/architecture/decision-runtime.md` before continuing.

## 17. Readiness review record

The 2026-09-16 document review closed the six implementation blockers:
conditional metadata (§9.1), architecture selection alignment (§6.2 and
architecture §8.1/§9), v1-to-v2 crash recovery (§9.2), budget/digest ownership
(§9.3), YAML write-back (§6.1), and compatibility-map authority (§4).
Implementation then completed Phases 0–5 in order. The checked definition of
done records the final code and validation state at commit `4242666`.
