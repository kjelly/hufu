# Hufu Cost Governance and Portable Team Packages

> Status: implemented
> Authority: normative
> Verified-Commit: `5665cc34e4eef23c6af93e370f1672f5168dc43c`
> Supersedes: —
> Superseded-By: —
> Implementation-Ready: yes; follow the PR boundaries and phase gates below
> Scope: `github.com/kjelly/hufu`
> Date: 2026-09-29

## 0. Decision

This plan contains only work that a coding agent can complete inside this
repository with deterministic tests and without an external service, a live
provider, or a maintainer product decision.

The implementation has three stages:

1. characterize the existing boundaries that the changes must preserve;
2. add generation-cost accounting, admission, and read-only projections;
3. add deterministic, inspectable, locally installable agent-team packages.

The following ideas from the superseded draft are deliberately not part of
this implementation plan:

- durable cross-process task leases;
- an event-driven wakeup consumer or scheduler;
- an Operator HTTP server or Web UI;
- cost-based execution-route selection;
- provider billing API integration;
- package download, registry, signature, or trust-store support.

Those features require a product or hosting decision that this repository does
not currently encode. A coding agent must not add them while implementing this
plan.

## 0.1 Expected benefits

### Generation-cost governance

- Operators can see estimated generation cost by run, task, role, execution
  target, and provider invocation.
- A configured hard run budget rejects a generation request before transport.
- Concurrent workers cannot each spend the same remaining in-process budget.
- Unknown, local, subscription, usage-derived, and conservative bounded costs
  remain distinguishable; unknown is never rendered as zero.
- Open reservations survive replay as conservative spend, so a crash does not
  silently restore budget that may already have been consumed.

### Portable team packages

- A team can be moved as one deterministic archive without workspace state.
- Package contents and hashes are inspectable before installation.
- Installation is staged, validated, collision-safe, and cannot escape its
  destination.
- Manifest-declared credentials, known credential files, symlinks, and runtime
  state fail closed instead of being silently archived.
- Pack/install round trips preserve the normalized `AgentTeam` configuration.

## 0.2 Normative language

`MUST`, `MUST NOT`, `SHOULD`, and `MAY` are normative. Examples are not a
substitute for the stated invariant.

---

# 1. Existing boundaries that remain authoritative

## 1.1 Runtime resource budget

`internal/team/budget_manager.go` remains the sole owner of token and wall-clock
resource accounting. Cost governance is a separate economic ledger. It MUST
not change the meaning of `TokensUsed`, `Reserved`, or `BudgetLimits`.

The cost manager is owned by the same root coordinator as the token ledger so
extra-model coordinators and parallel workers share one in-process run budget.

## 1.2 Provider admission

`internal/agent/provider_request.go` is the shared boundary for Fantasy-backed
generation calls:

```text
AdmitProviderRequest
    -> acquire provider slot
    -> CommitProviderInvocation
    -> provider transport
```

Cost admission MUST extend this boundary instead of adding a second model-call
path. The four entry points `Generate`, `Stream`, `GenerateObject`, and
`StreamObject` MUST have identical reservation and settlement semantics.

External agent backends such as Codex app-server do not necessarily cross the
Fantasy wrapper. Their `ExecutionBackend.RunAttempt` boundary requires an
explicit adapter described in section 5.6.

## 1.3 Usage truth

`internal/team.ExecutionUsage` remains the runtime usage shape. It already
distinguishes:

- uncached input;
- cache reads;
- cache creation/writes;
- output;
- provider-reported total.

Cost code MUST adapt this shape explicitly. It MUST NOT replace it with a
second task-attempt usage type. V1 does not add reasoning-token accounting
because the current provider contract does not expose it consistently.

## 1.4 Durable truth

The existing `EventStore` remains the canonical economic event log. The V1
cost implementation MUST NOT add a second JSONL ledger or SQLite authority.

Task `ExecutionReceipt.Usage` remains the usage evidence for that task attempt.
V1 MUST NOT add a `Cost` field to `ExecutionReceipt`; doing so would omit
coordinator and auxiliary invocations and create two competing cost truths.

The cost projection is rebuilt only from canonical cost events. Existing
execution receipts and old event logs remain readable without backfill.

## 1.5 Read-only inspection

`hufu inspect` remains the read-only operator facade. Cost inspection MUST use
`OpenEventStoreReadOnly` or an already borrowed read-only store. It MUST NOT:

- create a workspace or event file;
- append an event;
- repair or migrate state;
- invoke a provider or verifier;
- release or rewrite an open reservation.

## 1.6 Team schema

`hufu.io/v1alpha1`, `kind: AgentTeam` remains the team authoring schema. A team
package wraps an existing team directory; it is not a replacement team schema.

Pack and install validation MUST call the existing team compiler. Package code
must not reimplement team normalization.

---

# 2. Global invariants

## INV-01 — Unknown is not zero

The following states are distinct:

```text
usage-derived estimate
admission-bound estimate
local / no marginal provider charge
subscription / no per-call price
unknown
```

Only a known numeric estimate contributes a numeric USD value. `nil` or an
explicit status represents unknown; a zero pointer value represents known
zero.

## INV-02 — Admission precedes transport

When a hard cost budget is configured, price resolution, request estimation,
reservation, and durable reservation append MUST succeed before provider
transport or an external app-server process starts.

## INV-03 — One provider invocation, one reservation, at most one settlement

`provider_invocation_id` is the cost identity. Replay of the same idempotency
key returns the persisted event. A different payload for the same key fails
closed.

## INV-04 — Open reservations remain charged

If a process exits after reservation but before settlement, replay counts the
full reserved amount as conservative spend. Resume MUST NOT release it merely
because no settlement event exists.

## INV-05 — Settlement failure never replays provider work

If transport may have started, a settlement-persistence failure degrades cost
integrity and denies later generation calls in that run. It MUST NOT reclassify
the provider result as safe to retry.

## INV-06 — Cost policy is frozen

The effective run budget, unknown-price policy, and relevant price snapshots
are part of the execution-policy snapshot. Resume with different effective
values follows the existing snapshot-drift behavior.

## INV-07 — Package content is structurally allowlisted

Pack starts from recognized team-owned file classes and typed references. It
MUST NOT recursively archive the entire team directory and then rely on a
denylist to remove unsafe content.

## INV-08 — Package integrity is not authenticity

Hashes detect archive corruption or inconsistent contents. They do not prove
who produced the package. `inspect` and `install --dry-run` MUST report
`authenticity: unverified`. V1 MUST NOT claim signature verification.

---

# 3. Phase 0 — Characterization baseline

Phase 0 changes tests and documentation only. Reuse existing fixtures and
tests; do not add duplicate goldens where an existing contract already proves
the same behavior.

## 3.1 Required characterization

Add or extend tests for:

1. `ExecutionUsage` JSON compatibility, including cache read/write fields.
2. token-budget reservation sharing across parallel workers and extra-model
   coordinators.
3. all four `admittedLanguageModel` methods calling admission and durable
   commit before the inner model.
4. stream cancellation releasing provider slots exactly once.
5. EventStore interprocess append, idempotency, branch scoping, corrupt-tail,
   replaced-file, write, and sync failure behavior.
6. execution-policy v4 snapshot compatibility and drift handling.
7. `hufu inspect` opening an existing workspace without mutation.
8. legacy and v1alpha1 team manifests normalizing identically.
9. compile/load behavior for top-level agent Markdown, team-owned skills,
   result-contract schemas, and trusted-static Go action source.

## 3.2 Baseline note

Add `docs/reference/cost-package-baseline.md` containing:

- verified commit, Go version, and OS;
- tests reused or added for each item above;
- model-call paths covered by Fantasy admission;
- external backend paths that bypass it;
- package-owned and external team resources;
- compatibility assumptions carried into phases 1 and 2.

The note records observed facts only. It MUST NOT promise features from later
phases.

## 3.3 Exit gate

Run separately:

```bash
go test ./...
go vet ./...
golangci-lint run
```

All commands must exit zero before Phase 1 begins.

---

# 4. Workstream A — Cost value model and configuration

## 4.1 Package ownership

Create a dependency-light package:

```text
internal/cost/
    money.go
    types.go
    catalog.go
    calculate.go
    projection.go
```

`internal/cost` MUST NOT import `internal/team`, provider implementations, CLI
packages, or filesystem-backed runtime state. Team adapters convert
`ExecutionUsage` into cost package inputs.

## 4.2 Money

Use signed 64-bit USD micros:

```text
1 USD = 1,000,000 micros
```

All authored monetary values are decimal strings. YAML numeric scalars and
`float64` MUST be rejected. Parsing accepts canonical non-negative decimal
strings with at most six fractional digits and no exponent, sign, whitespace,
or currency symbol.

Examples:

```text
valid:   "0", "0.20", "2.000001"
invalid: 2.0, "-1", "+1", "1e-3", "$2", " 2"
```

Token-price multiplication uses checked integer arithmetic. Division by one
million rounds up so admission and reporting never understate a positive
fractional micro. Overflow and malformed input fail closed.

## 4.3 Types

The implementation may rename private fields, but the public persisted shape
must express these semantics:

```go
type BillingMode string

const (
    BillingMetered      BillingMode = "metered"
    BillingLocal        BillingMode = "local"
    BillingSubscription BillingMode = "subscription"
    BillingUnknown      BillingMode = "unknown"
)

type EstimateSource string

const (
    EstimateUsage         EstimateSource = "usage"
    EstimateAdmissionBound EstimateSource = "admission_bound"
    EstimateNotMetered    EstimateSource = "not_metered"
    EstimateSubscription  EstimateSource = "subscription"
    EstimateUnknown       EstimateSource = "unknown"
)

type TokenUsage struct {
    InputTokens         int64 `json:"input_tokens"`
    CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
    CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
    OutputTokens        int64 `json:"output_tokens"`
    TotalTokens         int64 `json:"total_tokens,omitempty"`
}

type PriceSnapshot struct {
    ID                          string      `json:"id"`
    ExecutionTarget             string      `json:"execution_target"`
    BillingMode                 BillingMode `json:"billing_mode"`
    InputMicrosPerMillion       *int64      `json:"input_micros_per_million,omitempty"`
    CacheReadMicrosPerMillion   *int64      `json:"cache_read_micros_per_million,omitempty"`
    CacheWriteMicrosPerMillion  *int64      `json:"cache_write_micros_per_million,omitempty"`
    OutputMicrosPerMillion      *int64      `json:"output_micros_per_million,omitempty"`
    OpaqueMaxMicrosPerInvocation *int64     `json:"opaque_max_micros_per_invocation,omitempty"`
    CatalogHash                 string      `json:"catalog_hash"`
}
```

Requirements:

- IDs and hashes are deterministic over canonical JSON, never timestamps.
- A metered entry must define either input/output token rates or an opaque
  per-invocation maximum. When token rates are used, both input and output are
  required. An entry may define both so an opaque backend can later expose
  complete usage without changing catalog identity.
- Omitted cache rates conservatively fall back to the uncached input rate.
- `local` and `subscription` entries must not define token rates.
- `unknown` is produced by resolution; it is not an authored price entry.
- `opaque-max-usd-per-invocation` is a conservative cap for an external agent
  backend whose internal token usage is not observable by Hufu.

## 4.4 Global catalog configuration

Add strict global/project `hufu.yaml` configuration:

```yaml
cost:
  prices:
    openai/gpt-example:
      billing-mode: metered
      input-usd-per-million: "2.00"
      cache-read-usd-per-million: "0.20"
      cache-write-usd-per-million: "2.00"
      output-usd-per-million: "8.00"

    ollama/qwen-example:
      billing-mode: local

    codex/gpt-example:
      billing-mode: metered
      opaque-max-usd-per-invocation: "0.10"
```

Rules:

- keys are canonical execution targets, not bare display model names;
- user and project catalogs merge by target, with the later complete entry
  replacing the earlier entry;
- V1 has no built-in cloud-price table and performs no network price lookup;
- provider credentials and base URLs never enter the catalog hash;
- strict decode rejects unknown keys and contradictory fields.

## 4.5 Team run policy

Add strict `cost:` policy to both legacy and v1alpha1 team schemas:

```yaml
cost:
  max-run-usd: "1.50"
  warning-run-usd: "1.00"
  unknown-price-policy: deny
```

Rules:

- `max-run-usd` and `warning-run-usd` are optional positive decimal strings;
- warning must not exceed max when both are set;
- `unknown-price-policy` is `allow` or `deny`;
- a hard max requires the effective policy to be `deny`; explicitly combining
  `max-run-usd` with `allow` is a validation error because it would make the
  advertised hard bound unenforceable;
- without a hard max, the default is `allow`; an explicit `deny` still permits
  an operator to require known pricing for every generation call;
- no CLI override is added in this plan;
- the policy applies to generation calls only; embeddings and non-model
  actions are outside V1 coverage and inspection reports
  `coverage: generation_only`.

## 4.6 Execution-policy snapshot

Bump the execution-policy snapshot writer version once. Keep the existing
version readable.

Add an optional cost block containing:

- max and warning micros;
- effective unknown-price policy;
- sorted price snapshots for every configured generation target;
- one deterministic cost-policy hash.

Omit the block when no price entry and no team cost policy exists so teams that
do not use the feature retain legacy behavior. A configured policy or catalog
change on resume is handled by the existing snapshot drift machinery.

## 4.7 Tests and acceptance

Tests must cover:

- decimal parsing and canonical formatting;
- sub-micro ceiling, zero, maximum value, and overflow;
- every billing mode and invalid field combination;
- cache-rate fallback;
- deterministic price and policy hashes;
- user/project merge semantics;
- strict decode for both config files and both team schemas;
- old execution-policy snapshot compatibility;
- cost-policy snapshot drift;
- no behavior or snapshot-shape change when cost is unused.

Acceptance gate A1:

```text
A1.1 No float participates in money parsing or calculation.
A1.2 Unknown is never represented as known zero.
A1.3 V1 prices come only from explicit configuration.
A1.4 Cost policy is frozen before the first provider call.
A1.5 Existing teams without cost configuration retain their old behavior.
```

---

# 5. Workstream A — Runtime ledger and admission

## 5.1 Canonical events

Add typed, versioned event payloads and stable event types:

```text
cost_price_snapshot_resolved
cost_reservation_committed
cost_settled
cost_budget_warning
cost_budget_denied
```

All idempotency keys are branch-scoped by the existing EventStore identity.
Use these logical keys. The initial `AdmitProviderRequest` pass may calculate a
diagnostic estimate, but the final concurrent budget check and durable
reservation occur in `CommitProviderInvocation`, after the existing wrapper
has assigned the provider invocation ID:

```text
cost-price:v1:<run-id>:<price-snapshot-id>
cost-reserve:v1:<run-id>:<provider-invocation-id>
cost-settle:v1:<run-id>:<provider-invocation-id>
cost-warning:v1:<run-id>:<threshold-micros>
cost-denied:v1:<run-id>:<provider-invocation-id>
```

Before the first reservation for a price snapshot, append
`cost_price_snapshot_resolved` idempotently through the root EventStore. It
contains the canonical, secret-free snapshot used for calculation. A snapshot
event without a later reservation is valid and contributes no spend.

While holding the manager mutex, event order is snapshot, optional first
warning-threshold crossing, then reservation. A warning append failure denies
that invocation before transport. A denied request appends
`cost_budget_denied` when possible; failure to append the denial never changes
the deny decision and the returned error reports both facts.

`cost_reservation_committed` contains:

- provider invocation ID;
- run, optional task, occurrence-attempt, agent, and purpose identity;
- canonical execution target and price snapshot ID;
- estimated request input tokens and reserved output tokens;
- reserved micros and estimate source;
- reservation timestamp supplied by the runtime clock.

`cost_settled` contains:

- the same identity;
- normalized observed usage when valid;
- final estimated micros;
- `usage` or `admission_bound` source;
- a bounded outcome category (`success`, `provider_error`, `cancelled`,
  `stream_abandoned`), never raw provider error text.

No prompt, response, tool argument, provider URL, credential, or raw error is
stored in a cost event.

## 5.2 Projection

`internal/cost.Projection` is a pure reducer. For each invocation:

```text
reservation without settlement -> reserved amount counts as spend
reservation plus settlement     -> settlement replaces reservation
duplicate equivalent event      -> no change
settlement without reservation  -> integrity error
different settlement for id     -> integrity error
```

Aggregate by:

- run;
- optional task;
- role/purpose;
- execution target;
- estimate source.

The projection exposes separate totals for usage-derived, admission-bound,
non-metered, subscription, and unknown invocations. It MUST NOT add unknown
invocations to a numeric zero total.

## 5.3 In-process manager

Add a coordinator-owned `CostManager` separate from `BudgetManager`. It holds:

- the frozen policy and price snapshots;
- a mutex-protected total of settled plus open reservations;
- provider invocation IDs already reserved or settled;
- an integrity-degraded latch.

Extra-model coordinators share the root manager and append through the root
coordinator's canonical EventStore, never a leaf workspace store. The manager
rehydrates the active branch and current top-level execution run from cost
events on resume before another provider request can be admitted. A resumed
task may retain an older occurrence `ExecutionReceipt.RunID`; new cost belongs
to the current top-level execution run that actually makes the billed call.

Reservation algorithm under a metered token price:

1. count the serialized request through the existing provider request
   counter;
2. take the maximum configured input/cache-read/cache-write rate because
   cache classification is not known before transport;
3. add `MaxOutputTokens` from the bound admission context or exact call;
4. calculate with checked, ceiling arithmetic;
5. under a hard budget, reject an unbounded or unknown estimate according to
   policy;
6. at the authoritative `CommitProviderInvocation` boundary, after a provider
   invocation ID exists and while holding the manager mutex, re-resolve the
   frozen estimate and verify
   `settled + open + requested <= max`;
7. append `cost_reservation_committed` durably;
8. publish the in-memory reservation only after append success.

For `local` and `subscription`, reservation is non-numeric and does not consume
the numeric hard budget, but the invocation is still recorded with its exact
mode. For an opaque metered backend, use the configured per-invocation maximum.

## 5.4 Settlement

Settlement rules:

- Valid non-zero component usage is priced with the frozen snapshot and marked
  `usage`.
- Missing, zero, contradictory, or unavailable usage commits the reserved
  amount and is marked `admission_bound`.
- Actual usage-derived cost may exceed the reservation if a provider violates
  its admitted output bound. Charge the larger actual value, emit the ordinary
  settlement, and deny later calls after the budget is exhausted.
- Settlement runs exactly once on response, provider error, stream exhaustion,
  early consumer stop, or parent-context cancellation.
- A post-transport settlement append failure latches degraded integrity and
  denies later calls. It does not transform the provider result into a
  retryable provider failure.
- On replay, an open reservation remains charged at its full amount.

Settlement persistence uses a bounded cleanup context derived with
`context.WithoutCancel`; it does not reuse a cancelled provider context. Any
asynchronous cleanup must be owned by the coordinator lifecycle and finish
before the EventStore closes.

If a caller neither iterates a returned stream nor cancels its parent context,
the runtime has no observable lifecycle end. The reservation remains open and
fully charged; V1 MUST NOT use a finalizer or timer to guess that the stream is
finished.

## 5.5 Fantasy-backed integration

Extend the existing optional provider invocation contract rather than wrapping
call sites independently. A suitable shape is:

```go
type ProviderInvocationSettler interface {
    SettleProviderInvocation(context.Context, ProviderRequest, ProviderInvocationResult)
}
```

The neutral result must contain only normalized usage, outcome category, and
whether a response/stream was observed. The team implementation owns durable
events and degraded-integrity state.

Update all four admitted-language-model methods. Streaming settlement must be
owned by a `sync.Once` cleanup shared by normal exhaustion, consumer stop, and
context cancellation. Existing provider-slot release semantics must remain
unchanged.

This integration covers:

- coordinator generation;
- normal Hufu-local workers;
- direct-agent execution;
- extra-model Hufu-local leaves;
- sidecar, guard, judge, skeptic, and plan-reviewer calls that use the admitted
  language-model wrapper;
- result-only protocol repair using the same wrapper.

Characterization tests, not filename assumptions, determine the final list.
Any generation chokepoint that bypasses both this wrapper and section 5.6 must
fail the coverage audit test.

## 5.6 External agent backends

`AgentExecutionBackend.RunAttempt` requires a cost adapter because an external
app-server can perform opaque internal work.

Before `RunAttempt` may prepare an execution world or start a process:

- generate a runtime-owned provider invocation ID using the same identity
  generator as the Fantasy wrapper;
- resolve the frozen target price;
- reserve `opaque-max-usd-per-invocation` for metered mode;
- deny under a hard budget if no opaque maximum exists;
- allow and record unknown only when no hard budget exists and policy allows;
- treat local/subscription modes as their explicit non-numeric modes.

After `RunAttempt`, settle from `AttemptResult.Usage` only when the usage is
valid and represents the complete external attempt. Otherwise settle the
opaque reservation. V1 MUST NOT claim that an opaque maximum is a provider
billing receipt.

## 5.7 Failure semantics

| Failure | Required result |
|---|---|
| Invalid catalog/policy | Fail startup before workspace lifecycle or provider access |
| Unknown/unbounded cost under deny policy | Deny before provider transport |
| Reservation event append fails | No provider call; return policy/admission error |
| Provider fails after reservation | Settle conservatively; preserve provider failure class |
| Settlement append fails | Keep provider outcome; latch cost integrity degraded; deny later calls |
| Crash after reservation | Replay counts full reservation |
| Projection corruption | Inspect reports integrity failure; runtime fails closed when a hard budget depends on it |

Cost denial is a policy admission result. It MUST NOT be fed into the existing
provider-failure fallback mechanism and MUST NOT select a cheaper route in V1.

## 5.8 Tests and acceptance

At minimum:

- concurrent reservations cannot oversubscribe one root manager;
- extra-model coordinators share the same manager;
- reservation append precedes every covered provider call;
- reservation append failure proves zero provider calls;
- each of the four Fantasy methods settles once;
- stream success, provider error, parent-context cancellation, early consumer
  stop, and a never-iterated stream that remains conservatively open;
- valid usage replaces reservation;
- missing/invalid usage commits the conservative bound;
- opaque external backend admission and settlement;
- no opaque cap plus hard budget fails before process start;
- settlement append failure never triggers a provider retry;
- crash/reopen counts an open reservation;
- duplicate event replay is stable;
- old workspaces with no cost events project as unavailable, not zero;
- coordinator, direct agent, worker, extra-model, and each auxiliary role is
  represented by the call-path coverage audit.

Acceptance gate A2:

```text
A2.1 Every in-scope generation transport has a pre-transport reservation.
A2.2 A hard budget cannot be oversubscribed by parallel work in one run.
A2.3 Open reservations remain conservative across crash/replay.
A2.4 Cost persistence failure never replays a possibly billed call.
A2.5 External opaque work is denied under a hard budget unless bounded.
A2.6 Token BudgetManager semantics are unchanged.
```

---

# 6. Workstream A — Read-only projections and operator output

## 6.1 CLI

Add:

```bash
hufu inspect cost [run-id] --workspace <workspace>
hufu inspect cost [run-id] --task <task-id> --format json
```

If run ID is omitted, use the same exact-scope resolution rules as other
inspect subcommands. Ambiguous scope is an error; do not guess from raw logs.

Text output includes:

```text
Coverage            generation_only
Run                 run-123
Usage-derived       $0.384200
Admission-bound     $0.100000
Open reservations   $0.020000
Unknown invocations 1
Local invocations   4
Subscription calls  0
Budget              $1.500000
Remaining           $0.995800
Integrity           ok
```

`Remaining` is omitted when there is no hard budget or integrity is not good.
Unknown and non-metered modes are never folded into the numeric total.

JSON uses an additive versioned envelope and integer micros. It includes the
freshness event ID/hash used by the projection.

## 6.2 Other projections

Add cost information only where it answers an existing operator question:

- optional `Cost` view on `OperatorSnapshot`;
- cost section in `--report`;
- cost line in the final execution summary when data is available;
- read-only Operator TUI panel rendering through the snapshot projection.

Do not add cost to model decision truth, task success criteria, or
`ExecutionReceipt` in V1. Do not add an HTTP endpoint.

## 6.3 Read-only tests

- inspect on a missing workspace fails without creating it;
- inspect does not append, migrate, repair, or call a provider;
- corrupt chains and unmatched settlements surface integrity failure;
- old workspace shows cost unavailable;
- text/JSON distinguish all estimate modes;
- redaction test proves no prompt, provider URL, or credential is exposed;
- OperatorSnapshot old golden remains compatible through an optional field;
- report, summary, and TUI use the same projection totals;
- branch and run filters cannot mix sibling lineage events.

Acceptance gate A3:

```text
A3.1 One cost projection supplies CLI, OperatorSnapshot, report, summary, and TUI.
A3.2 Read-only inspection causes no persistent mutation.
A3.3 Freshness and integrity are explicit.
A3.4 No UI surface derives cost from prose or legacy logs.
```

---

# 7. Workstream B — Portable agent-team package

## 7.1 Package and command ownership

Create:

```text
internal/teampkg/
    manifest.go
    inventory.go
    pack.go
    inspect.go
    install.go
    paths.go
    secrets.go

cmd/hufu/
    team_package_cmd.go
```

Commands:

```bash
hufu team pack <team-directory> --version <label> --output <file.hufu>
hufu team package inspect <file.hufu> [--format text|json]
hufu team install <file.hufu> [--global] [--dry-run]
```

Rules:

- `pack` never overwrites an existing output file;
- package `name` is the normalized team name produced by the existing compiler;
  V1 has no rename override;
- the version label matches `[A-Za-z0-9][A-Za-z0-9._+-]{0,127}` and is not a
  semantic version range in V1;
- install defaults to `<cwd>/.agent-teams/<name>`;
- `--global` selects `~/.agent-teams/<name>` and is mutually exclusive with
  any future explicit target flag;
- install never overwrites an existing team;
- no command downloads a package or resolves a registry.

## 7.2 Archive format

V1 uses ZIP with `zip.Store` and this layout:

```text
package.yaml
TEAM.lock.json
team.yaml                 # or team.yml, matching the source
<top-level agent>.md
README.md                 # optional
skills/<skill>/...
<referenced schema files>
<trusted-static Go action source>/*.go
```

`package.yaml`:

```yaml
schema_version: 1
name: example-team
version: 0.1.0
team_manifest: team.yaml
authenticity: unverified
```

No build time, hostname, username, absolute path, or random ID is stored.

Deterministic archive rules:

- entries sorted by normalized slash path;
- directories omitted;
- file mode normalized to `0644`;
- ZIP timestamp fixed to `1980-01-01T00:00:00Z`;
- Store method, empty extra fields and comment;
- exact source bytes preserved;
- generated YAML/JSON uses stable field and list order and a trailing newline.

The reproducibility guarantee is: identical allowed input bytes, version
label, team name, and Hufu package schema produce identical archive bytes.

## 7.3 Structural inventory

Inventory begins from these allowed sources only:

1. exactly one `team.yaml` or `team.yml`;
2. top-level regular `*.md` files, including optional `README.md`;
3. regular files beneath a team-owned `skills/<name>/` whose root contains a
   valid `SKILL.md`;
4. team-relative result-contract schema paths discovered by the existing team
   compiler;
5. non-test `.go` files in each trusted-static Go action source directory,
   matching `internal/golangruntime` inspection semantics.

The inventory MUST NOT include:

- project/global skills merely visible during compilation;
- `required-resources` that point into the subject project;
- command executables or command-provider working directories;
- MCP server binaries;
- provider credentials or environment values;
- workspace/session/log/context databases;
- files found only by following prose references.

Pack compiles the team first, builds the typed inventory, then compiles a
staged copy containing only that inventory. The staged normalized team config
must equal the source normalized config. If removing an unowned file changes
compilation, pack fails and names the unsupported dependency.

External executables, project resources, MCP servers, and non-team skills stay
external runtime requirements. `inspect` reports them; the package does not
pretend to vendor them.

## 7.4 Path and size rules

Every entry path must:

- be valid UTF-8 and non-empty;
- use normalized `/` separators;
- be relative, clean, and contain no `.` or `..` segment;
- contain no NUL, backslash, absolute prefix, or Windows drive/UNC prefix;
- be unique under both exact and Unicode-independent ASCII case-folded
  comparison;
- remain below the package root after platform-native conversion.

V1 limits:

```text
maximum entries:                 2048
maximum uncompressed file size:  8 MiB
maximum total uncompressed size: 64 MiB
maximum archive size:            64 MiB
maximum compression ratio:       100:1 for accepted third-party ZIP entries
```

Pack rejects every symlink, hard-link-like non-regular entry, socket, device,
FIFO, and nested archive path escape. Install validates the same rules before
creating its staging directory.

## 7.5 Lock file

`TEAM.lock.json` has schema version 1 and contains the hash and size of every
entry except itself:

```json
{
  "schema_version": 1,
  "team_digest": "sha256:...",
  "files": [
    {"path": "package.yaml", "sha256": "...", "size": 96},
    {"path": "team.yaml", "sha256": "...", "size": 1234}
  ]
}
```

The file list is path-sorted. `team_digest` hashes the versioned domain string
and each tuple `path NUL sha256 NUL decimal-size NUL` in order. It is not an
archive signature.

Inspect/install reject missing, extra, duplicate, size-mismatched, or
hash-mismatched entries.

## 7.6 Secret and runtime-state policy

Structural policy is the primary control; content scanning is defense in
depth.

Scan raw authored files and typed YAML fields before template interpolation,
and archive those raw authored bytes. The pack path MUST NOT resolve an
environment placeholder and then serialize its value into the archive or
generated metadata.

Always reject:

- `.env`, `.env.*`, known private-key names, SSH keys, credential stores, and
  files beneath runtime-state directories;
- a non-empty literal `provider-api-key` at any supported manifest layer;
- literal secrets in provider, backend, or MCP environment mappings;
- PEM private-key blocks and high-confidence known token prefixes;
- absolute paths in package-owned typed references;
- symlinks even when they resolve inside the team directory.

Environment placeholders may name a variable but must not carry its resolved
value into generated package metadata. Secret diagnostics show path, field,
and finding category only; they never echo the candidate secret.

There is no `--allow-secret` bypass in V1. Acceptance claims prevention for
structurally declared and high-confidence known secrets, not detection of every
possible secret hidden in arbitrary prose.

## 7.7 Inspect

`team package inspect` is read-only with respect to the package source,
workspace, and team search paths. It may use an automatically removed temp
directory to run the existing compiler.

Text and JSON report:

- package schema, name, and version;
- archive SHA-256 and team digest;
- `authenticity: unverified`;
- manifest schema version;
- file count and sizes;
- normalized team name and agent list;
- included team-owned skills, result schemas, and Go actions;
- external executable, MCP, project-resource, and skill requirements;
- secret/path findings;
- compile result.

## 7.8 Install transaction

Install performs:

```text
read and bound archive
    -> validate paths, duplicates, sizes, and entry types
    -> strict-decode package metadata and lock
    -> verify every hash
    -> run secret/runtime-state scan
    -> extract into a temp directory beside the target
    -> compile and validate the staged team
    -> verify package name equals the staged normalized team name
    -> fsync regular files and staging directory where supported
    -> atomically rename staging directory to the absent target
```

`--dry-run` performs every validation. Temporary scratch writes are allowed
and removed; it makes no persistent target, workspace, registry, or config
change.

Failure cleanup removes only the exact staging directory created by that
invocation. It never removes or rewrites an existing target.

## 7.9 Tests and acceptance

At minimum:

- identical source produces byte-identical packages;
- source byte, version, or name change changes digest;
- legacy and v1alpha1 manifests;
- top-level agent Markdown and optional README;
- team skill with nested resource files;
- result-contract schema inclusion;
- trusted-static Go action includes exactly its runtime-owned `.go` files;
- external project resource remains a reported requirement;
- source staged compile equals packaged staged compile;
- missing/extra/duplicate/hash-mismatched entry;
- absolute, parent, backslash, drive, UNC, NUL, and case-fold collision paths;
- symlink inside, symlink escape, device/non-regular entry;
- entry/file/total/archive/compression-ratio limits;
- literal provider key, MCP environment secret, private key, and token finding;
- diagnostics do not echo secret content;
- inspect makes no persistent mutation;
- project install and global install target resolution;
- target collision and atomic rename failure preserve the existing filesystem;
- interrupted install leaves no published partial team;
- dry-run produces no persistent target;
- installed team compiles to the same normalized config.

Acceptance gate B:

```text
B1. Archive input is structural allowlist, not recursive copy-minus-denylist.
B2. Same inputs produce the same archive bytes.
B3. Lock verification rejects any content mismatch.
B4. Extraction cannot escape or collide across supported platforms.
B5. Install never overwrites or partially publishes a team.
B6. Pack -> install -> compile preserves normalized TeamConfig.
B7. Authenticity is explicitly unverified; no signature claim is made.
```

---

# 8. PR plan

Each item is an independent review boundary. Do not combine workstreams into a
single PR.

## CP-000 — Characterization baseline

- reuse/add Phase 0 tests;
- add `docs/reference/cost-package-baseline.md`;
- no production behavior change.

## CP-101 — Cost types, parser, calculator, and catalog

- `internal/cost` pure value layer;
- global catalog strict config and merge;
- unit and compatibility tests;
- no runtime calls or events.

## CP-102 — Team cost policy and frozen snapshot

- legacy/v1alpha1 strict team fields;
- policy validation;
- execution-policy version bump and compatibility;
- no provider admission yet.

## CP-103 — Cost events, projection, and manager

- typed event payloads and reducers;
- replay and open-reservation semantics;
- root coordinator manager;
- no provider wiring yet.

## CP-104 — Fantasy provider admission and settlement

- reservation at the existing provider boundary;
- settlement across all four model methods;
- stream lifecycle tests;
- call-path coverage audit.

## CP-105 — External backend cost boundary

- opaque maximum admission;
- `AgentExecutionBackend` integration;
- pre-process fail-closed tests;
- no route selection behavior.

## CP-106 — Cost inspect and operator projections

- inspect command and JSON/text schema;
- OperatorSnapshot, report, summary, and TUI projection;
- read-only and redaction tests.

Gate A1, A2, and A3 must all pass before the cost workstream is complete.

## CP-201 — Team package manifest, inventory, and deterministic pack

- structural inventory;
- metadata/lock schemas;
- deterministic ZIP;
- source/staged compile equivalence.

## CP-202 — Package path, limit, and secret validation

- archive bounds;
- path/collision rules;
- secret/runtime-state checks;
- adversarial fixtures.

## CP-203 — Package inspect

- text/JSON views;
- external requirement projection;
- read-only temp compilation.

## CP-204 — Atomic package install

- project/global target resolution;
- dry run;
- staging, compile, fsync, and rename;
- collision and interruption tests.

Gate B must pass before the package workstream is complete. CP-201 may begin
after CP-000 and does not depend on cost work.

---

# 9. Per-PR completion contract

Every implementation PR must state:

1. problem and canonical owner;
2. invariant introduced or preserved;
3. exact persistence boundary;
4. pre-side-effect admission point;
5. old-workspace/config/package compatibility;
6. failure and cancellation behavior;
7. tests and fault injection;
8. observable projection changes;
9. rollback strategy.

For every code PR, run separately:

```bash
go test ./...
go vet ./...
golangci-lint run
```

All must exit zero. Focused tests are useful during development but never
replace the full gates.

Additional rules:

- no test depends on a live provider, Paperclip checkout, package registry, or
  external service;
- use fake providers and process fixtures already present in the repository;
- use `internal/processutil.StartAndWait` for any new subprocess with
  `Pdeathsig` behavior;
- background settlement/telemetry goroutines join the coordinator lifecycle
  wait group before stores close;
- no new top-level executable or server;
- no new dependency unless the standard library and existing dependencies
  cannot implement the accepted contract;
- no implementation step may modify a concrete built-in team or personal
  configuration to enable the feature.

---

# 10. Program completion criteria

The plan is complete only when:

- [x] every in-scope generation path is covered by reservation and settlement;
- [x] hard cost denial occurs before provider/process side effect;
- [x] parallel work cannot oversubscribe the in-process run budget;
- [x] usage-derived, admission-bound, local, subscription, and unknown remain
      distinct through replay and every operator projection;
- [x] open reservations remain conservative after crash/reopen;
- [x] cost persistence failure cannot cause provider replay;
- [x] old workspaces and snapshots remain readable;
- [x] team package input is structurally allowlisted;
- [x] deterministic package bytes and lock verification pass;
- [x] package inspection states that authenticity is unverified;
- [x] install is bounded, traversal-safe, collision-safe, staged, compiled,
      and atomically published;
- [x] pack/install preserves normalized team configuration;
- [x] no Task Lease, Wakeup daemon, HTTP API, Web UI, cost-based route
      selection, provider billing integration, registry, or signature system
      was introduced;
- [x] `go test ./...`, `go vet ./...`, and `golangci-lint run` pass.

## End
