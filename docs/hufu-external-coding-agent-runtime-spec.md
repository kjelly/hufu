# Hufu External Coding Agent Runtime Specification

> **Status:** Implemented and live-verified against a real Codex account (all §36 PRs, §37's mandatory integration test matrix, and §38's opt-in smoke suite pass) — spec verified against codebase 2026-09-08
> **Primary target:** Codex via `codex app-server`
> **Future targets:** Claude Code, OpenCode, other coding-agent runtimes
> **Hufu baseline:** `kjelly/hufu` commit `29d8e54d7e10414e8828efb79301338d980ea2b3`
> **Codex protocol reference baseline:** `openai/codex` commit `4b0f44d3046f5212e618d0b5fb5225a2f1988989`
> **Normative language:** MUST / MUST NOT / SHOULD / MAY are requirements in the RFC sense.

---

## 0. Executive Summary

Hufu SHALL support external coding agents as interchangeable **SubagentProvider** implementations.

The first production provider SHALL be Codex and SHALL use:

1. `codex app-server` over JSON-RPC stdio;
2. a Hufu-owned `ExecutionWorld`;
3. durable provider/session binding;
4. Codex `outputSchema` to request a structured **untrusted `WorkerResultProposal`**;
5. Hufu-owned workspace observation and artifact hashing;
6. Hufu-owned `ExecutionReceipt`, verification, retry, recovery and completion;
7. explicit cancellation and process-tree cleanup;
8. crash-safe `thread/resume`.

The core invariant is:

```text
External coding agent = execution engine, not runtime authority.
```

Codex MAY inspect and modify an authorized workspace and MAY run local development commands, but it MUST NOT decide or forge:

- task acceptance;
- verification success;
- receipt validity;
- artifact identity or hashes;
- task/run identity;
- retry/recovery policy;
- final task lifecycle state;
- memory promotion;
- decision-runtime policy;
- external side-effect authorization.

The final execution flow SHALL be:

```text
Coordinator
  → durable task occurrence
  → durable SubagentProvider binding
  → ExecutionWorld.Prepare
  → provider.RunAttempt
      → Codex app-server
      → thread/start or thread/resume
      → turn/start(outputSchema=WorkerResultProposal)
      → terminal turn/completed
      → thread/read final state
  → Hufu workspace delta
  → Hufu canonical TaskResult
  → Hufu ExecutionReceipt
  → verification
  → retry / recovery / reconciliation
  → CompletionGate
  → common run finalization
```

This specification extends the existing runtime-composability design. It MUST NOT create a second task scheduler, completion system, receipt format or recovery state machine.

---

# 1. Current Hufu State

At the baseline commit, Hufu already has the correct high-level seam.

## 1.1 Existing `SubagentProvider`

`internal/team/subagent_provider.go` already defines:

```go
type SubagentProvider interface {
    AttemptRunner
    Name() string
    Capabilities() SubagentCapabilities
}
```

and documents that the provider executes exactly one already-authorized attempt while Hufu retains:

- retry;
- recovery;
- verification;
- receipts;
- completion;
- memory policy.

This contract SHALL remain the primary abstraction.

## 1.2 Existing provider registry

`internal/team/subagent_registry.go` already provides a fail-closed registry.

The current default provider name is:

```text
hufu-local
```

Unknown providers already fail before execution.

This behavior MUST remain.

## 1.3 Existing local implementation

`internal/team/subagent_hufu.go` already adapts the Fantasy runtime into `SubagentProvider`.

It re-resolves canonical task/agent/tool state and does not trust the caller-provided attempt surface.

This behavior is the semantic baseline against which the external provider path SHALL be tested.

## 1.4 Current missing boundary

`internal/team/coordinator_task_run.go` still resolves:

```go
c.SubagentRegistry().Resolve(localSubagentProviderName)
```

for production worker attempts.

Therefore the registry exists, but provider selection is not yet a durable task property.

Separately, the one-worker direct-agent fast path (`coordinator_run.go`'s `createDirectAgent`) does not call `SubagentRegistry` or `HufuLocalSubagentProvider` at all today — it is a structurally distinct code path that builds a Fantasy agent inline. This is a second, independent missing boundary, not covered by the fix above. It is addressed by §28.1.

## 1.5 Existing trusted runtime state

The following existing concepts SHALL remain Hufu-owned:

- `TodoItem`
- `TaskOccurrenceProjection`
- `ExecutionReceipt`
- `TaskResult`
- `VerificationResult`
- `FailureEventPayload`
- `SideEffectClass`
- `RecoveryPolicy`
- `CompletionGate`
- EventStore / EventJournal
- task occurrence admission
- decision runtime
- worker memory / experience processing

No external provider may write these objects directly into canonical runtime state.

---

# 2. Goals

The implementation SHALL achieve all of the following.

## 2.1 Provider-neutral external worker execution

Hufu MUST be able to select:

```text
hufu-local
codex
<future provider>
```

without modifying coordinator scheduling semantics.

Adding a future provider MUST NOT require changes to:

- retry loop;
- task lifecycle;
- verification;
- CompletionGate;
- event reducers;
- memory promotion;
- decision routing.

Only provider registration/configuration and the provider driver SHOULD change.

## 2.2 Codex as a real coding worker

Codex SHALL be allowed to use its native coding workflow inside the authorized execution world, including:

- reading repository files;
- searching source;
- editing files when the task permits workspace writes;
- running local compiler/test/lint commands;
- iterating until it produces its final proposal.

Hufu MUST NOT reduce Codex to a one-shot text-generation model.

## 2.3 Durable provider/session identity

A durable task occurrence MUST remember which external provider owns it.

A retry or resume MUST NOT silently change:

```text
codex → hufu-local
codex → another provider
```

because configuration changed after the occurrence was admitted.

## 2.4 Untrusted provider result

Codex's final structured response SHALL be an untrusted proposal.

Hufu SHALL independently determine:

- actual modified files;
- actual artifacts;
- hashes;
- verification result;
- task/run identity;
- execution receipt.

## 2.5 Crash-safe Codex resume

Once Codex creates a persistent thread, the thread ID SHALL be persisted in the task occurrence/receipt event stream before Hufu relies on it for subsequent continuation.

A process restart SHALL be able to start a fresh app-server process and use `thread/resume`.

## 2.6 Provider isolation

A failure in Codex SHALL not corrupt the scheduler.

Codex process failure SHALL produce an `AttemptResult` / classified execution error and return control to Hufu recovery logic.

---

# 3. Non-goals

The first implementation SHALL NOT:

1. make Codex a coordinator;
2. allow the coordinator LLM to arbitrarily choose a provider;
3. let Codex call Hufu's internal Go interfaces directly;
4. trust Codex-reported hashes, receipts or verification;
5. expose unrestricted host filesystem access;
6. use `danger-full-access` in production;
7. automatically approve interactive privilege escalations;
8. implement GitHub/Kubernetes/cloud write capability directly through Codex native shell;
9. replace Hufu decision routing with Codex collaboration/multi-agent behavior;
10. migrate Hufu's own local provider away from Fantasy;
11. require one long-lived global Codex app-server daemon;
12. treat Codex thread history as canonical Hufu state.

Hufu MCP/tool brokering for privileged external effects is a later extension. Version 1 SHALL fail closed instead of granting those effects natively.

---

# 4. Architectural Invariants

Every PR implementing this specification MUST preserve these invariants.

## INV-01 — Hufu owns lifecycle

Only Hufu can transition:

```text
pending
planned
in_progress
protocol_incomplete
verifying
done
error
blocked
paused
skipped
```

Provider output is evidence, not a transition command.

## INV-02 — Provider cannot accept a task

A provider returning `success` does not make a Todo `done`.

The normal pipeline remains:

```text
provider success claim
→ canonicalize
→ verification
→ Hufu transition
→ run acceptance / CompletionGate
```

## INV-03 — Provider binding is immutable after admission

Once a durable occurrence has a provider binding, provider resolution MUST read the durable occurrence.

Live team/agent configuration MUST NOT retarget the occurrence.

## INV-04 — Provider identity and model identity are separate

Hufu MUST distinguish:

```text
worker provider: codex
Codex model: gpt-...
Hufu coordinator model: ...
```

Provider routing MUST NOT be overloaded into `TaskDef.Model`.

## INV-05 — Result proposal is untrusted

The provider MUST NOT populate trusted fields of canonical `TaskResult`.

## INV-06 — Receipts are Hufu-issued

Codex event data MAY contribute observations, but an `ExecutionReceipt` is only valid if constructed/persisted by Hufu.

## INV-07 — Workspace truth comes from Hufu observation

Actual changed files and content hashes SHALL be determined by Hufu before/after snapshots, not by provider prose.

## INV-08 — No external-provider `danger-full-access`

If a provider requires unrestricted native access, provider admission MUST fail.

## INV-09 — Cancellation preserves evidence

Cancellation MUST retain:

- partial provider output if available;
- provider thread/turn identity;
- usage known so far;
- transcript reference;
- workspace delta observed so far;
- failure classification.

## INV-10 — Unknown provider fails before side effects

The existing registry fail-closed rule MUST remain.

## INV-11 — Durable event before relying on durable binding

New provider/session binding state required for crash recovery MUST be committed through EventJournal before later logic assumes it exists.

## INV-12 — Decision policy remains Hufu-owned

External coding execution MUST not bypass configured JUDGE / CHALLENGE / REVISE / commit-gate discipline.

---

# 5. Target Architecture

```text
┌──────────────────────────────────────────────────────┐
│ Hufu Coordinator                                     │
│                                                      │
│ scheduling / task contract / decision discipline     │
│ retry / recovery / reconciliation                    │
│ verification / acceptance / CompletionGate           │
└───────────────────────┬──────────────────────────────┘
                        │
                        ▼
             ResolvedSubagentBinding
                        │
                        ▼
               SubagentRegistry
                 │             │
                 │             │
                 ▼             ▼
          hufu-local         codex
          provider           provider
                               │
                               ▼
                     ExecutionWorld
                               │
                               ▼
                     codex app-server
                         JSON-RPC stdio
                               │
             ┌─────────────────┴─────────────────┐
             │ thread/start / thread/resume      │
             │ turn/start / turn/interrupt       │
             │ turn/completed + thread/read      │
             └─────────────────┬─────────────────┘
                               │
                               ▼
                    WorkerResultProposal
                         (UNTRUSTED)
                               │
                               ▼
                  Hufu canonicalization
                   ├─ workspace snapshot/diff
                   ├─ artifact hashing
                   ├─ runtime identity
                   ├─ receipt
                   └─ verification
```

---

# 6. Configuration Schema

## 6.1 Team-level provider definitions

Add a provider registry configuration to `agent.TeamConfig`.

Proposed YAML:

```yaml
subagent-provider-default: hufu-local

subagent-providers:
  codex:
    type: codex-app-server
    command: [codex, app-server]
    protocol: app-server-v2

    startup-timeout: 15s
    interrupt-grace: 3s
    shutdown-grace: 3s

    max-event-bytes: 1048576
    max-transcript-bytes: 16777216

    execution-world: local-sandbox

    # Explicit process environment only.
    inherit-env:
      - PATH
      - CODEX_HOME

    # Never infer or inject OPENAI_API_KEY automatically.
    # Authentication should normally come from pre-authenticated CODEX_HOME.
```

Reserved provider:

```text
hufu-local
```

MUST NOT be overridden by `subagent-providers`.

### Go types

Add to `internal/agent/agent.go`:

```go
type SubagentProviderConfig struct {
    Type               string        `yaml:"type" json:"type"`
    Command            []string      `yaml:"command" json:"command"`
    Protocol           string        `yaml:"protocol" json:"protocol"`
    StartupTimeout     string        `yaml:"startup-timeout" json:"startup_timeout"`
    InterruptGrace     string        `yaml:"interrupt-grace" json:"interrupt_grace"`
    ShutdownGrace      string        `yaml:"shutdown-grace" json:"shutdown_grace"`
    MaxEventBytes      int64         `yaml:"max-event-bytes" json:"max_event_bytes"`
    MaxTranscriptBytes int64         `yaml:"max-transcript-bytes" json:"max_transcript_bytes"`
    ExecutionWorld     string        `yaml:"execution-world" json:"execution_world"`
    InheritEnv         []string      `yaml:"inherit-env" json:"inherit_env"`
}

type TeamConfig struct {
    ...
    SubagentProviderDefault string                            `yaml:"subagent-provider-default"`
    SubagentProviders       map[string]SubagentProviderConfig `yaml:"subagent-providers"`
}
```

`TeamConfig` itself carries no `yaml` struct tags today. YAML parsing goes through a separate intermediate struct in `internal/team/parse.go` (the struct backing keys like `max-rounds`), followed by an explicit field-by-field copy into `TeamConfig` (e.g. `cfg.MaxRounds = yc.MaxRounds`). The `yaml` tags shown above on `TeamConfig` are illustrative of the target field name only, not a literal instruction to tag `TeamConfig` directly.

`SubagentProviderDefault` and `SubagentProviders` MUST be added to that same intermediate parse struct and copied across in that same place, following the existing pattern. Adding the tags only to `TeamConfig` will compile but silently fail to populate the fields from YAML. Do not add an independent YAML loader, and do not assume struct tags on `TeamConfig` alone are sufficient.

## 6.2 Agent default provider

Add to `agent.AgentDef`:

```go
SubagentProvider string
```

Agent frontmatter:

```yaml
subagent-provider: codex
```

Parsing MUST be added to the existing agent frontmatter parser in `internal/team/parse.go`.

## 6.3 Static task provider pin

Add to `TaskDef`:

```go
// SubagentProvider is configuration-owned. The coordinator model cannot
// choose or lower it at dispatch time.
SubagentProvider string `json:"-" yaml:"subagent-provider,omitempty"`
```

This follows the same protection principle as:

- `DecisionProfile`
- `DecisionOptions`
- static `Action`
- workflow `Phase`

The coordinator task JSON MUST NOT expose the field.

## 6.4 Provider selection precedence

For a newly admitted durable task:

```text
TaskDef.SubagentProvider
  > AgentDef.SubagentProvider
  > TeamConfig.SubagentProviderDefault
  > hufu-local
```

The result SHALL be normalized to lowercase and validated against `SubagentRegistry`.

Resolution MUST occur before first provider/model execution.

Unknown provider:

```text
→ fail closed
→ no provider process
→ no model call
→ no workspace side effect
```

---

# 7. Durable Provider Binding

## 7.1 New binding type

Add:

```go
type ProviderBinding struct {
    Provider          string `json:"provider"`
    Protocol          string `json:"protocol,omitempty"`

    // Stable provider-side reasoning/session identity.
    SessionID         string `json:"session_id,omitempty"`

    // Last active provider turn, diagnostic only.
    TurnID            string `json:"turn_id,omitempty"`

    // Effective provider-reported identity.
    ProviderVersion   string `json:"provider_version,omitempty"`
    EffectiveModel    string `json:"effective_model,omitempty"`

    // Hufu-owned execution world identity.
    ExecutionWorldID  string `json:"execution_world_id,omitempty"`

    // Frozen task workspace root/cwd assertion.
    CWD               string `json:"cwd,omitempty"`

    // Provider-native sandbox projection used for this binding.
    SandboxMode       string `json:"sandbox_mode,omitempty"`

    ResumeSupported   bool   `json:"resume_supported,omitempty"`
}
```

`ProviderBinding` MUST NOT contain credentials or secrets.

## 7.2 Persist on Todo

Add to:

- `TodoItem`
- `TodoSpec`
- `TaskOccurrenceProjection`
- canonical task shadow/parity structs
- task-created event payload
- reducer
- session/checkpoint projection
- replay comparison

fields:

```go
SubagentProvider string           `json:"subagent_provider,omitempty"`
ProviderBinding  *ProviderBinding `json:"provider_binding,omitempty"`
```

`SubagentProvider` is immutable after task admission.

`ProviderBinding.SessionID` MAY transition from empty to populated after `thread/start`.

## 7.3 Task occurrence contract comparison

`SubagentProvider` MUST be part of durable occurrence contract equality.

A live scheduler `TaskDef` that disagrees with durable provider binding MUST be rejected exactly as other durable occurrence contract mismatches are rejected.

Provider session ID MUST NOT be part of static task contract equality; it is runtime execution state.

## 7.4 New event types

Add typed events:

```text
task_provider_bound
provider_session_bound
provider_turn_started
provider_turn_terminal
execution_world_prepared
execution_world_released
provider_protocol_failure
provider_result_proposed
provider_result_canonicalized
```

Minimum `provider_session_bound` payload:

```go
type ProviderSessionBoundPayload struct {
    TaskID           string `json:"task_id"`
    Attempt          int    `json:"attempt"`
    Provider         string `json:"provider"`
    Protocol         string `json:"protocol"`
    SessionID        string `json:"session_id"`
    ExecutionWorldID string `json:"execution_world_id"`
    CWD              string `json:"cwd"`
}
```

Reducers MUST be idempotent by occurrence identity / event idempotency key.

Session binding MUST be persisted before starting a later retry/resume that depends on it.

---

# 8. Provider Interfaces

## 8.1 Extend capability description

Replace the current simple capability set with provider-neutral observable capabilities.

```go
type SubagentCapabilities struct {
    SupportsHufuTools    bool `json:"supports_hufu_tools"`
    SupportsTypedResult  bool `json:"supports_typed_result"`
    SupportsActivities   bool `json:"supports_activities"`
    SupportsResumeToken  bool `json:"supports_resume_token"`

    SupportsNativeFS     bool `json:"supports_native_fs"`
    SupportsNativeShell  bool `json:"supports_native_shell"`
    SupportsOutputSchema bool `json:"supports_output_schema"`
    SupportsInterrupt    bool `json:"supports_interrupt"`
}
```

Capabilities describe behavior; they MUST NOT grant authorization.

For Codex v1:

```go
SubagentCapabilities{
    SupportsHufuTools:    false,
    SupportsTypedResult:  false, // canonical TaskResult is Hufu-only
    SupportsActivities:   true,
    SupportsResumeToken:  true,
    SupportsNativeFS:     true,
    SupportsNativeShell:  true,
    SupportsOutputSchema: true,
    SupportsInterrupt:    true,
}
```

## 8.2 Attempt request

Extend `AttemptRequest`:

```go
type AttemptRequest struct {
    ...
    Provider          string
    ProviderBinding   *ProviderBinding
    ExecutionWorld    *PreparedExecutionWorld
}
```

The provider MUST treat these as assertions from Hufu.

For durable tasks, provider implementations SHOULD re-check canonical task identity through a narrow Hufu service where possible.

## 8.3 Attempt result

Extend `AttemptResult`:

```go
type AttemptResult struct {
    Output            string
    TypedResult       *TaskResult // hufu-local compatibility only

    ResultProposal    *WorkerResultProposal

    Usage             ExecutionUsage
    StepsUsed         int
    StopReason        string

    ProviderSessionID string
    ProviderTurnID    string
    TranscriptRef     string

    ProviderBinding   *ProviderBinding
}
```

For external providers:

```text
TypedResult MUST be nil.
ResultProposal MAY be non-nil.
```

Hufu-local MAY continue returning canonical typed results through the existing path.

---

# 9. Untrusted `WorkerResultProposal`

## 9.1 Purpose

External providers MUST NOT deserialize directly into `TaskResult`.

Create:

```go
type WorkerResultProposal struct {
    Status        string         `json:"status"`
    Summary       string         `json:"summary"`
    Details       string         `json:"details,omitempty"`

    ProposedFiles []ProposedFile `json:"proposed_files,omitempty"`
    FilesRead     []string       `json:"files_read,omitempty"`
    Findings      []Finding      `json:"findings,omitempty"`
    Risks         []Risk         `json:"risks,omitempty"`
    OpenQuestions []string       `json:"open_questions,omitempty"`
    Facts         map[string]any `json:"facts,omitempty"`

    Confidence    float64        `json:"confidence,omitempty"`
}

type ProposedFile struct {
    Path        string `json:"path"`
    Description string `json:"description,omitempty"`
    Role        string `json:"role,omitempty"`
}
```

`Finding` and `Risk` MUST reuse the existing `internal/team` types already defined for canonical `TaskResult` (see `task_result.go`: `Finding{Category,Summary,Detail}`, `Risk{Description,Impact,Mitigation}`). Implementations MUST NOT declare second, differently-shaped `Finding`/`Risk` types in the same package — that would be a package-level redeclaration. If the existing shapes are insufficient for proposal use, extend them (additively) rather than forking a parallel type.

**Added 2026-09-08**: `FilesRead` is exactly such an additive extension, not part of the original shape — added to close the gap §38 documented (a task whose verify-spec asserts canonical `TaskResult`'s `/files_read`, e.g. `hufu-code-review`'s `review-workset`, was otherwise unsatisfiable through an external provider at all). Each claimed path is untrusted, like every `ProposedFiles` entry: verified against the observed workspace delta first, then the live filesystem, before becoming a canonical `FileRef`; an unverifiable claim is dropped for a non-grounded task and fails a grounded one closed (§9.4's `provider_claimed_missing_file` treatment, applied the same way here).

The proposal MUST NOT contain:

- TaskID
- RunID
- Attempt
- Agent
- Provider
- SHA256
- byte size as authority
- evidence HMAC
- receipt IDs
- verification
- `ExecutionReceipt`
- authoritative artifact IDs
- source/trust classification

Any such fields supplied by the provider MUST cause strict schema decode failure.

## 9.2 Status values

Allowed proposal statuses:

```text
success
completed_with_gaps
partial
failed
blocked
```

The same semantic meaning as existing Hufu `TaskResult` statuses SHOULD be preserved.

## 9.3 Canonicalization

Add:

```go
type ExternalResultCanonicalizer interface {
    Canonicalize(
        ctx context.Context,
        request AttemptRequest,
        result AttemptResult,
        delta WorkspaceDelta,
    ) (*TaskResult, error)
}
```

The default canonicalizer SHALL:

1. validate provider binding;
2. validate proposal schema;
3. assign canonical task/run/attempt/agent/provider identity;
4. use Hufu-observed changed files;
5. resolve proposed artifact paths only inside authorized workspace;
6. hash actual artifact bytes;
7. create Hufu-owned `ArtifactRef`;
8. drop provider claims for nonexistent files;
9. attach proposal findings/risks/facts only after size/schema limits;
10. set a distinct source:

```text
external_provider_proposal
```

11. never create verification claims;
12. never mark the Todo done.

## 9.4 Proposal vs actual workspace mismatch

Hufu SHALL compare:

```text
proposal.ProposedFiles
vs
WorkspaceDelta
```

Mismatch classes:

```text
provider_omitted_modified_file
provider_claimed_missing_file
provider_claimed_outside_workspace
provider_claimed_unchanged_file
```

For tasks with grounded-result requirements:

- outside-workspace claims MUST fail canonicalization;
- nonexistent artifact claims MUST fail canonicalization;
- omitted modified files SHALL be recorded from Hufu truth and MAY produce a warning;
- Hufu truth always wins.

---

# 10. ExecutionWorld

## 10.1 Requirement

An external provider that can execute native shell/filesystem operations MUST run inside a Hufu-owned execution-world contract.

Add:

```go
type ExecutionWorld interface {
    Name() string
    Capabilities() ExecutionWorldCapabilities

    Prepare(
        context.Context,
        ExecutionWorldSpec,
    ) (*PreparedExecutionWorld, error)

    Snapshot(
        context.Context,
        *PreparedExecutionWorld,
    ) (WorkspaceSnapshot, error)

    Release(
        context.Context,
        *PreparedExecutionWorld,
    ) error
}
```

## 10.2 Spec

```go
type ExecutionWorldSpec struct {
    RunID      string
    TaskID     string
    Attempt    int

    Root       string
    CWD        string

    SideEffect SideEffectClass

    WritableRoots []string
    ReadOnlyRoots []string

    NetworkAllowed bool

    EnvironmentAllowlist []string

    MaxOutputBytes int64
}
```

## 10.3 Prepared world

```go
type PreparedExecutionWorld struct {
    ID            string
    Provider      string

    Root          string
    CWD           string

    WritableRoots []string
    ReadOnlyRoots []string

    NetworkAllowed bool

    Environment    []string

    Baseline       WorkspaceSnapshot
}
```

No password/token value may be persisted in this object.

## 10.4 First implementation

Add:

```text
internal/team/execution_world.go
internal/team/execution_world_local.go
```

Name:

```text
local-sandbox
```

Version 1 MAY use the existing working tree rather than creating a full VM/container, but it MUST:

- freeze absolute cwd;
- freeze writable roots;
- sanitize process environment;
- take a baseline workspace snapshot;
- require the provider driver to project the world into a native sandbox;
- reject `danger-full-access`;
- detect changes outside allowed writable roots after execution;
- fail closed if native sandbox projection cannot be proven.

It MUST NOT claim Firecracker/container-level isolation.

## 10.5 Side-effect mapping

Codex v1 SHALL map:

| Hufu side effect | Codex native sandbox |
|---|---|
| `none` | `read-only` |
| `workspace_write` | `workspace-write` |
| `external_write` | `workspace-write`; external effect unavailable in v1 |
| `infra_mutation` | `workspace-write`; infra mutation unavailable in v1 |
| `credential_mutation` | reject provider admission |

`danger-full-access` MUST NOT be used.

## 10.6 Network policy

Hufu owns network intent.

If Hufu effective policy says `no-net`, Codex network MUST be disabled.

If network is allowed by Hufu, the driver MAY enable network only when the selected Codex sandbox policy supports an explicit network field and the driver can verify that projection.

If the installed Codex protocol version cannot express/verify the requested network constraint:

```text
fail closed
```

Do not silently downgrade to unrestricted network.

## 10.7 Environment policy

Do not use:

```go
os.Environ()
```

for the external provider process.

Construct environment from an explicit allowlist.

Default minimum SHOULD be:

```text
PATH
CODEX_HOME
```

`HOME` SHOULD point to an isolated Hufu runtime home, not the user's normal home, unless explicitly configured.

Hufu MUST NOT automatically pass:

```text
OPENAI_API_KEY
AWS_*
GITHUB_TOKEN
GH_TOKEN
KUBECONFIG
DATABASE_URL
SSH_AUTH_SOCK
```

Codex authentication SHOULD use a pre-authenticated `CODEX_HOME`.

Explicit secret forwarding, if added later, requires a separate credential-broker design and is outside v1.

---

# 11. Workspace Snapshot and Delta

## 11.1 Snapshot interface

Add:

```go
type WorkspaceSnapshotter interface {
    Snapshot(
        context.Context,
        *PreparedExecutionWorld,
    ) (WorkspaceSnapshot, error)

    Diff(
        context.Context,
        WorkspaceSnapshot,
        WorkspaceSnapshot,
    ) (WorkspaceDelta, error)
}
```

## 11.2 Snapshot data

```go
type WorkspaceFileState struct {
    Path   string
    SHA256 string
    Bytes  int64
    Mode   uint32
}

type WorkspaceSnapshot struct {
    ID          string
    Root        string
    CapturedAt  time.Time
    ManifestRef string
    Digest      string
}

type WorkspaceDelta struct {
    Added    []WorkspaceFileState
    Modified []WorkspaceFileState
    Deleted  []string
}
```

Large manifest content SHOULD be stored as an artifact; events keep only digest/reference.

## 11.3 Git optimization

When the workspace is a Git working tree, implementation SHOULD use Git to discover candidates efficiently, but Hufu SHALL hash actual file bytes itself.

Do not trust only `git status` for artifact integrity.

## 11.4 Non-Git fallback

A directory snapshot fallback MUST exist for tests and non-Git workspaces.

It SHOULD:

- skip `.git`;
- skip Hufu internal artifact/log directories;
- avoid symlink traversal outside root;
- enforce a maximum file count / total metadata budget;
- return a typed failure if safe snapshotting is impossible.

## 11.5 Outside-root mutation

If post-attempt inspection detects a mutation outside allowed writable roots:

```text
FailurePolicy / security violation
→ no successful canonical result
→ no task completion
→ provider is not automatically retried
```

---

# 12. Codex Provider

Add package/files:

```text
internal/team/subagent_codex.go
internal/team/codex_appserver_client.go
internal/team/codex_appserver_protocol.go
internal/team/codex_result_schema.go
internal/team/codex_process.go
```

Provider name:

```text
codex
```

Provider type:

```text
codex-app-server
```

---

# 13. Codex App-server Process Model

## 13.1 One app-server process per Hufu attempt

Version 1 SHALL use:

```text
one app-server process
per provider RunAttempt call
```

Reasons:

- failure isolation;
- simple stdout framing;
- simple cancellation;
- no global multiplexing state;
- easier process-tree cleanup;
- parallel Hufu attempts can independently run;
- Codex thread persistence still permits resume in a fresh process.

A future optimization MAY pool app-server processes only after parity tests prove no lifecycle change.

## 13.2 Process startup

Use argv, never shell interpolation.

Example configured argv:

```text
["codex", "app-server"]
```

Hufu SHALL:

1. validate executable presence;
2. create pipes;
3. start process in its own process group/job;
4. apply sanitized environment;
5. start bounded stderr capture;
6. start JSON-RPC stdout reader;
7. send `initialize`;
8. require successful initialize response before thread operations.

Startup timeout SHALL be enforced.

## 13.3 Protocol compatibility

The driver SHALL target app-server v2 methods used by the Codex reference baseline.

**Verified 2026-09-08 against the real, installed `codex-cli 0.153.4`** (`codex app-server`, both its own `generate-json-schema` output and a live stdio round trip — not a guess): method names below match exactly, but every request/response field is **camelCase**, not the snake_case earlier drafts of this spec assumed (`threadId` not `thread_id`, `outputSchema` not `output_schema`, and so on). `internal/team/codex_appserver_protocol.go` implements the verified shapes; treat this section's field names as the corrected reference.

Required methods:

```text
initialize
thread/start
thread/resume
turn/start
turn/interrupt
```

`thread/read` exists in the real protocol but is not required for the driver's own turn-completion flow — see the corrected §13.4 below. It remains available for future diagnostic/history use.

Required terminal notification:

```text
turn/completed
```

The driver SHOULD consume useful notifications such as:

```text
item/started
item/completed
item/commandExecution/outputDelta
turn/plan/updated
```

but MUST NOT require every non-terminal notification for correctness. This was directly confirmed live: a single real turn/start call emitted a long, evolving stream of advisory notifications (`remoteControl/status/changed`, `thread/started`, `deprecationNotice`, half a dozen `mcpServer/startupStatus/updated`, `account/rateLimits/updated`, `item/started`/`item/completed`, ...) that the driver correctly ignores by matching only on notification method `turn/completed`.

Unknown notifications SHALL:

- be bounded by max-event-bytes;
- be preserved in diagnostic transcript when allowed;
- otherwise be ignored;
- never crash the provider.

A missing terminal condition MUST be a protocol failure.

## 13.4 The final answer comes from `turn/completed` itself, not a separate `thread/read`

**Corrected 2026-09-08** (this subsection previously assumed the opposite — that streaming/notification content could never be trusted for the final answer and a separate `thread/read` was required; live verification against the real app-server showed this was wrong):

The real `turn/completed` notification's own payload already carries a full `Turn` object, including its `items` array — and that array already contains the terminal `agentMessage` item (`phase: "final_answer"`) whose `text` is exactly the `outputSchema`-constrained JSON Hufu asked for. There is no separate step needed to obtain it.

Do not use `thread/read` with full-history hydration (`includeTurns: true`) for this purpose: the real app-server itself emits a `deprecationNotice` recommending `thread/turns/list` + `thread/items/list` pagination instead, for exactly the full-history case `thread/read` used to cover. The driver's own turn-completion flow never needs any of these — it reads the final answer directly out of the `turn/completed` notification's embedded `turn.items`.

Extraction rule: scan `turn.items` from the end for the last item with `type == "agentMessage"` and `phase == "final_answer"`; its `text` is the raw JSON to decode as `WorkerResultProposal`. A turn whose `status` is not `"completed"` (e.g. `"failed"`, `"interrupted"`) MUST NOT be treated as a candidate for extraction at all — classify it as a process/protocol failure using the terminal `Turn.error`, never attempt to parse `items`.

Hufu runtime state remains canonical outside the provider.

---

# 14. Codex Thread Lifecycle

## 14.1 First attempt

When no durable provider session exists:

```text
initialize
→ thread/start
→ persist provider_session_bound
→ turn/start
→ wait for turn/completed (final answer already embedded, §13.4)
```

`thread/start` SHALL use (field names verified live, 2026-09-08 — camelCase, see §13.3):

```text
cwd: PreparedExecutionWorld.CWD
approvalPolicy: never
sandbox: read-only | workspace-write        # plain string on the request side
model: Hufu-selected external-worker model if configured
developerInstructions: the §14.4 protocol-only instruction (thread-scoped, see §14.4)
```

`runtimeWorkspaceRoots` is not a `thread/start` request field in the real protocol — it appears only in the *response*, server-derived from the app-server's own config, not something Hufu's request can set. Do not send it.

Hufu MUST verify the returned effective:

- cwd (`response.cwd`, a plain string);
- sandbox (`response.sandbox` is a **policy object** in the real protocol, e.g. `{"type":"readOnly","networkAccess":false}` — map its `type` back to Hufu's plain `read-only`/`workspace-write`/`danger-full-access` string before comparing against the requested mode);
- model (`response.model`, a plain string);
- provider identity where available.

The thread id itself is nested at `response.thread.id`, not a flat `thread_id`.

A mismatch that widens permissions MUST fail before the coding turn.

## 14.2 Retry/resume

When `ProviderBinding.SessionID` exists:

```text
initialize fresh app-server
→ thread/resume(threadId)
→ verify returned cwd/sandbox/model
→ turn/start(retry feedback)
```

Use `threadId` resume, not unstable history/path injection. Verified live: resuming a thread that never had any turn run on it fails (`"no rollout found for thread id ..."`) — the real app-server only persists a resumable rollout once at least one turn has completed. This does not affect the normal retry/resume flow (§7.4's durable session binding is always persisted only after `thread/start`'s own effective-state validation succeeds, and by the time any later attempt resumes it, either a turn already ran or the crash-recovery paths in §24 apply), but it is a known edge case worth keeping in mind for any future protocol work touching thread lifecycle before a turn ever starts.

The binding's SessionID is provider context only; it does not replace Hufu task replay.

## 14.3 Model binding

The external worker model MUST be resolved by Hufu before provider invocation.

Codex MUST NOT silently substitute an arbitrary model.

If Codex reports a different effective model than the bound one:

```text
fail closed
```

unless a future explicit provider policy authorizes model fallback and records it durably before execution.

For v1:

```text
provider model fallback = disabled
```

## 14.4 Turn input

**Corrected 2026-09-08**: the real `turn/start` has no per-turn developer-instruction field at all. `input` is a `UserInput[]` array (Hufu sends exactly one `{"type":"text","text":"..."}` element — the already-compiled Hufu worker prompt), and the protocol-only instruction described below is a **thread-scoped** field (`developerInstructions`, on `thread/start`/`thread/resume`), applying to every turn run on that thread for as long as it lives — not something resent per turn.

The provider MAY set a short provider-specific developer instruction containing only protocol requirements, for example:

```text
- Work only inside the provided workspace.
- Produce the final response matching the supplied JSON schema.
- Do not claim verification or receipt authority.
```

It MUST NOT duplicate Hufu task scheduling or invent new policy. A result-only repair (§23) that resumes a thread with a forced read-only sandbox sets its own, different `developerInstructions` on that resume call, overriding the original task's for the repair thread's lifetime.

## 14.5 `outputSchema`

Every normal Codex coding turn MUST set Codex `turn/start.outputSchema` to the strict schema for `WorkerResultProposal`.

The schema MUST:

- set required `status` and `summary`;
- enumerate allowed status values;
- disallow unknown fields;
- bound nested structures where JSON Schema permits;
- not expose trusted Hufu fields.

**Verified 2026-09-08 against the real OpenAI structured-output validator** behind codex-cli 0.153.4 (confirmed by the exact `invalid_json_schema` errors it returns for a non-conforming schema, live): "disallow unknown fields" and "bound nested structures" are not enough on their own. Two additional constraints apply, recursively, to every object in the schema including nested ones:

1. every object needs `"additionalProperties": false` explicitly present — not merely implied, and not `true`;
2. every key listed in an object's `"properties"` must also appear in its `"required"` array. There is no true "optional" property in strict mode. A field that is logically optional is instead typed as `["<type>", "null"]`, and a well-behaved model emits JSON `null` for "not present".

A genuinely free-form/open-ended object (unknown key set decided at runtime) cannot be expressed under constraint 1 at all — `WorkerResultProposal.Facts` (§9.1) has no way to satisfy this while remaining truly free-form, so the schema Hufu sends only ever allows `facts` to be `null` or `{}`: an external (Codex) provider's proposal does not populate `Facts` in v1. `Facts` remains available on `WorkerResultProposal` for other decode paths that don't route through this schema.

The final assistant result is decoded with Go's strict JSON decoder (`DisallowUnknownFields` or equivalent) regardless of what the wire-level `outputSchema` already constrained — that decode is Hufu's actual trust boundary, this schema only reduces how often a well-behaved model produces something that fails it.

---

# 15. Codex Approval Policy

Version 1 SHALL set:

```text
approvalPolicy = never
```

because Hufu, not Codex interactive UI, owns unattended authorization.

If Codex requests a permission escalation despite this policy:

```text
→ reject / do not approve
→ classify provider policy violation or sandbox denial
→ retain transcript
→ return to Hufu recovery
```

Hufu MUST NOT emulate "press yes" behavior.

A future integration MAY route approval requests into Hufu policy gates, but that is a separate phase and MUST produce Hufu-owned receipts.

---

# 16. Cancellation and Process Cleanup

## 16.1 Required flow

On context cancellation or attempt timeout:

```text
if turn ID known:
    send turn/interrupt(thread_id, turn_id)

wait interrupt-grace

if process still active:
    terminate provider process group

wait shutdown-grace

if descendants remain:
    force-kill process tree
```

## 16.2 Process supervisor

Add a provider-neutral abstraction:

```go
type ProcessSupervisor interface {
    Start(context.Context, ProcessSpec) (ProcessHandle, error)
    Interrupt(context.Context, ProcessHandle) error
    TerminateTree(context.Context, ProcessHandle) error
    KillTree(context.Context, ProcessHandle) error
}
```

On platforms where reliable descendant cleanup is unavailable, unattended external provider mode MUST fail preflight rather than pretend cleanup is guaranteed.

Unix implementation SHOULD use a dedicated process group.

Windows SHOULD use a Job Object implementation before external provider mode is considered production-supported there.

## 16.3 Partial evidence

Before returning cancellation, Hufu SHALL attempt to preserve:

- bounded stderr;
- raw provider transcript artifact;
- thread ID;
- turn ID;
- last activity;
- token usage if known;
- post-cancel workspace snapshot.

---

# 17. Transcript Handling

## 17.1 Raw provider transcript

JSON-RPC traffic and relevant provider activities SHOULD be persisted as an opaque, bounded artifact.

The transcript MUST be redacted using Hufu's existing redaction utilities before durable persistence.

EventStore MUST only store:

- artifact reference;
- digest;
- bounded preview if policy allows.

Do not put full Codex transcript into EventStore payloads.

## 17.2 Size limits

Enforce:

```text
max-event-bytes
max-transcript-bytes
max-stderr-bytes
```

A single oversized JSON-RPC frame MUST terminate the provider as a protocol violation.

Truncated diagnostics MUST explicitly record truncation.

---

# 18. Provider Activity → Hufu Status Mapping

Codex notifications MAY be mapped to Hufu `StatusEvent` for UI/TUI observability.

Examples:

```text
item/started(command execution)
→ tool_call-like activity, provider=codex

item/commandExecution/outputDelta
→ bounded activity text

item/completed
→ provider activity completed

turn/plan/updated
→ informational provider-plan status only
```

These events MUST NOT produce Hufu tool receipts.

Codex native shell commands are provider-internal activities.

Hufu receipt truth is workspace/result/verification based.

UI MUST visually distinguish:

```text
Hufu tool call
vs
Codex internal command
```

to avoid implying the Codex command passed through Hufu's tool-policy gate.

---

# 19. Hufu Canonical TaskResult Construction

After provider termination:

```text
postSnapshot = ExecutionWorld.Snapshot()
delta = Diff(baseline, postSnapshot)

proposal = AttemptResult.ResultProposal

canonical = ExternalResultCanonicalizer.Canonicalize(
    request,
    proposal,
    delta,
)
```

Canonical `TaskResult` SHALL use:

```go
TaskID     = request.TaskID
Attempt    = request.Attempt
Agent      = canonical durable agent
Status     = validated proposal status
Summary    = proposal.Summary
Details    = proposal.Details
Findings   = proposal.Findings
Risks      = proposal.Risks
Facts      = bounded validated proposal.Facts
Source     = "external_provider_proposal"
```

`FilesModified` SHALL come from actual delta.

`Artifacts` SHALL come from validated proposed file paths intersected with actual workspace state.

`Evidence`, `ReceiptIDs`, and `Verification` are populated only later by Hufu-owned logic.

---

# 20. ExecutionReceipt Changes

Existing receipt semantics SHALL remain.

Add provider provenance fields:

```go
type ExecutionReceipt struct {
    ...
    SubagentProvider   string `json:"subagent_provider,omitempty"`
    ProviderSessionID  string `json:"provider_session_id,omitempty"`
    ProviderTurnID     string `json:"provider_turn_id,omitempty"`
    ExecutionWorldID   string `json:"execution_world_id,omitempty"`
    WorkspaceBefore    string `json:"workspace_before,omitempty"`
    WorkspaceAfter     string `json:"workspace_after,omitempty"`
}
```

These are Hufu-written fields.

`ProducerID` MAY continue to contain the isolated worker identity, but MUST NOT be overloaded as the only provider/session field.

Receipt finalization must still occur in Hufu's current attempt pipeline.

---

# 21. Verification

External provider completion MUST pass existing Hufu verification semantics.

No special Codex success shortcut is allowed.

Examples:

```text
Codex says success
+ go test fails
→ verification failure
→ retry/recovery path

Codex says success
+ required artifact absent
→ verification failure

Codex says partial
+ tests pass
→ task remains incomplete because provider did not claim terminal completion

Codex says failed
→ execution failure / recovery policy
```

For objective coding tasks, team definitions SHOULD continue using:

- `verify`
- `verify-spec`
- acceptance specifications
- grounded-result assertions

---

# 22. Retry Semantics

## 22.1 Retry ownership

Hufu decides whether retry is allowed.

Codex MUST NOT self-increment Hufu retries.

## 22.2 Thread reuse

Default retry policy for Codex:

```text
same task occurrence
+ same provider binding
+ safe retry
→ resume same Codex thread
```

Retry prompt SHALL include:

- prior canonical failure classification;
- Hufu verification failure;
- concise actual workspace state;
- instruction to correct the existing workspace.

## 22.3 Fresh thread cases

Hufu MAY deliberately discard provider session and start a new thread only when a typed recovery rule requires a fresh attempt.

This action MUST:

- emit a durable event;
- retain old session ID in receipt history;
- never erase prior evidence.

## 22.4 Unsafe replay

If a task may have performed an external side effect, Hufu recovery policy remains authoritative.

Because v1 Codex has no privileged external-effect channel, these tasks SHOULD normally fail admission before native external mutation is possible.

---

# 23. Result-only Protocol Repair

An invalid/missing `WorkerResultProposal` MUST NOT cause Hufu to rerun the full coding task immediately.

Add a bounded external result-repair path.

Flow:

```text
workspace already changed
→ proposal invalid/missing
→ freeze task execution effects
→ resume same Codex thread
→ force read-only sandbox
→ send schema-only result repair prompt
→ outputSchema=WorkerResultProposal
→ max one normal repair
```

The repair turn MUST NOT have workspace-write permission.

If repair still fails:

```text
TaskProtocolIncomplete
```

and existing Hufu resume/recovery semantics take over.

A repair proposal cannot retroactively change observed workspace history.

---

# 24. Crash Recovery

## 24.1 Crash before provider session persisted

If `thread/start` succeeded but Hufu crashed before `provider_session_bound` was durably committed, Hufu MUST NOT assume the unknown Codex session.

The task is treated according to the actual side-effect/workspace recovery state.

## 24.2 Crash after session binding but before turn terminal event

On resume:

1. replay task/provider binding from EventStore;
2. inspect task occurrence/recovery state;
3. start a fresh app-server process;
4. `thread/resume`;
5. inspect provider thread state;
6. reconcile workspace;
7. only continue/retry if Hufu recovery policy permits.

Do not automatically send a new coding turn solely because the thread exists.

## 24.3 Crash after Codex mutation but before Hufu verification

Workspace state and task receipt/recovery rules remain authoritative.

Hufu MUST run reconciliation/verification, not blindly replay the coding attempt.

---

# 25. Provider Preflight

Before scheduling the first Codex attempt, validate:

```text
provider registered
command executable exists
app-server initialize works
required protocol methods available
execution world available
process-tree cleanup available
task side-effect compatible
sandbox projection available
model binding valid
workspace root valid
CODEX_HOME/auth preconditions satisfied
```

Preflight failure MUST occur before the task enters provider execution.

The preflight result SHOULD be cacheable by provider binary/version but MUST be invalidated when executable identity changes.

**Scoped 2026-09-08** (implementation note, not a correction of the list above): the implemented preflight (`codexPreflightCache`/`codexRunCheapPreflightChecks` in `subagent_codex.go`) only independently re-verifies the checks above that need no subprocess: `command executable exists`, `execution world available`, `workspace root valid`, and (when `CODEX_HOME` is inherited) a best-effort `CODEX_HOME/auth preconditions satisfied` heuristic (directory exists and contains an `auth.json`; the file's content is never read). `provider registered` and `task side-effect compatible`/`model binding valid` are enforced elsewhere in the dispatch path before `RunAttempt` is ever called, not inside preflight itself.

The remaining items (`app-server initialize works`, `required protocol methods available`, `process-tree cleanup available`, `sandbox projection available`) are deliberately NOT independently re-probed via a second, throwaway app-server process. Live verification against the real app-server (codex-cli 0.153.4, 2026-09-08 — see §26's own "Corrected" note and §13.3) confirmed it has no side-effect-free capability-introspection RPC: a dedicated preflight probe could only repeat the exact same `initialize` handshake that `RunAttempt`'s own `StartCodexAppServer` call already performs as its first step — and that call already fails closed, before any workspace side effect, if the binary or protocol is actually broken. A second probe process would only double per-attempt process-launch cost without adding a real safety property, and — in the test harness — would desynchronize the single-`initialize`-per-attempt assumption every fake-server-script-driven test is built on. See `codexPreflightCache`'s doc comment for the full reasoning.

---

# 26. Codex Version / Protocol Drift

Codex app-server is an evolving protocol.

Hufu SHALL NOT bind correctness to an arbitrary Codex CLI version string alone.

The driver SHALL use feature/protocol preflight.

Required capability checks:

```text
initialize succeeds
thread/start accepted
thread/resume accepted
turn/start supports outputSchema
turn/interrupt accepted or advertised
thread/read available
read-only sandbox available
workspace-write sandbox available
```

If a field/method required for a Hufu safety invariant disappears:

```text
provider unavailable
```

Do not silently remove the invariant.

Unknown additive notifications MAY be tolerated.

**Scoped 2026-09-08**: these capability checks are enforced inline, at the point each method is actually used during a real attempt (`StartCodexAppServer`'s `initialize` call, `thread/start`/`thread/resume`, the `turn/start` request's `outputSchema` field, `turn/interrupt`, the sandbox policy accepted by `thread/start`/`thread/resume`) — each failing the attempt closed with a classified `provider unavailable`-equivalent error the moment the real app-server rejects or omits what Hufu needs, per §13.3/§13.4's live-verified protocol shape. There is no separate, dedicated feature-detection probe that runs before scheduling and independently exercises this same list against a second throwaway process; see §25's "Scoped" note for why (no side-effect-free introspection RPC exists on the real app-server, and a second probe would desynchronize the single-`initialize`-per-attempt fake-server test harness). `thread/read available` is superseded by §13.4: the driver does not call `thread/read` for the final-answer path at all, so that specific check has no runtime equivalent to enforce.

---

# 27. Subagent Registry Construction

During coordinator/service construction:

```text
NewSubagentRegistry(hufuLocal)
→ register configured external providers
→ validate default provider
```

Registration SHALL be deterministic.

Duplicate normalized provider name:

```text
configuration error
```

Reserved names:

```text
hufu-local
```

cannot be configured externally.

External provider constructor MUST NOT execute Codex/model calls during config parsing; only structural validation occurs there.

Runtime preflight performs executable/protocol checks.

---

# 28. Direct Agent / Fast Path

The one-worker direct-agent fast path MUST obey the same provider binding rules.

It MUST NOT bypass external provider support by always creating a Fantasy agent.

Required parity:

```text
coordinated task using coder/codex
≈
direct fast-path coder/codex
```

for:

- provider binding;
- execution-world policy;
- result canonicalization;
- receipt;
- verification;
- finalization.

If direct execution cannot preserve these semantics in the first PR, Codex-backed workers MUST be excluded from direct fast path and escalated to the normal coordinator task path until parity is implemented.

Do not create a second simplified Codex path.

## 28.1 Guard timing (MUST NOT be deferred to PR-15)

At baseline, `coordinator_run.go`'s direct-agent fast path (`createDirectAgent`) has zero dependency on `SubagentRegistry` or `HufuLocalSubagentProvider` — it unconditionally builds a Fantasy agent inline. Because of this, "the first PR" in the exclusion rule above MUST NOT be read as "PR-15" (Phase 7). Read literally that way, Phases 5-6 would enable real Codex execution while the direct path remains completely unguarded — a `subagent-provider: codex` agent run through the direct path would silently bypass every invariant in this specification.

The minimal fail-closed guard:

```text
if resolved provider != hufu-local:
    direct fast path MUST reject/escalate, not execute
```

MUST land no later than PR-03 (Phase 1, when provider resolution first becomes a durable occurrence property) and MUST remain in force through Phase 5/6. Full parity (provider binding, execution-world policy, result canonicalization, receipt, verification, finalization all working through the direct path) MAY still wait for PR-15; only the reject/escalate guard is time-critical.

---

# 29. Extra-model Fanout

Hufu's current `ModelTopology` is separate from provider topology.

Version 1 SHALL NOT run one Codex provider occurrence per `ExtraModels` leaf unless that path is explicitly updated and tested.

Safe initial rule:

```text
external provider + ModelTopology length > 1
→ fail preflight as unsupported
```

or route only after an explicit implementation phase.

Do not silently reinterpret Hufu local model fanout as Codex internal multi-agent behavior.

---

# 30. Decision Runtime Compatibility

Provider binding occurs after the task's trusted static contract is formed.

Decision discipline MUST remain outside provider.

For tasks with decision profiles:

```text
prepareTaskDecision
→ decision routing / commit discipline
→ provider attempt
```

must preserve the current runtime ordering.

Codex's own plan/collaboration capabilities MUST NOT satisfy Hufu:

```text
JUDGE
REFERENCE
CHALLENGE
REVISE
```

unless Hufu explicitly routes those roles through its normal capability resolver as separate Hufu agent occurrences.

---

# 31. Security Requirements

## SEC-01

Never use `danger-full-access` for production Codex workers.

## SEC-02

Never inherit the entire Hufu environment.

## SEC-03

Never trust provider-created artifact IDs/hashes.

## SEC-04

Never trust provider verification claims.

## SEC-05

Never auto-approve Codex privilege requests.

## SEC-06

Never expose Hufu HMAC/evidence signing secret to provider process.

## SEC-07

Never persist raw credentials in provider binding, EventStore or transcript.

## SEC-08

Symlinks resolving outside authorized roots MUST NOT be accepted as result artifacts.

## SEC-09

Provider output paths MUST be canonicalized and root-checked.

## SEC-10

External provider protocol frames MUST have hard size limits.

## SEC-11

Provider stderr MUST be secret-redacted before durable persistence.

## SEC-12

If process-tree cleanup cannot be guaranteed for unattended execution, fail preflight.

---

# 32. Telemetry

Add provider-neutral telemetry fields:

```text
subagent_provider
subagent_protocol
provider_session_id_hash
execution_world
provider_turn_status
provider_protocol_error
provider_result_repair
provider_workspace_added_count
provider_workspace_modified_count
provider_workspace_deleted_count
```

Do not emit raw session IDs if telemetry leaves the local workspace; use a stable hash where appropriate.

Status/TUI SHOULD display:

```text
worker: coder
runtime: codex
model: ...
sandbox: workspace-write
```

This makes Hufu agent identity distinct from execution provider identity.

---

# 33. Failure Classification

Map provider failures into existing Hufu failure taxonomy where possible.

Add typed provider sub-reasons if needed:

```text
provider_unavailable
provider_protocol_error
provider_process_crash
provider_sandbox_mismatch
provider_result_invalid
provider_result_missing
provider_cancelled
provider_timeout
provider_workspace_violation
provider_model_mismatch
provider_resume_failed
```

Do not classify every provider error as generic worker failure.

Retry policy SHALL distinguish:

### transient candidates

```text
process crash before side effect
temporary app-server transport failure
response stream disconnect where workspace reconciliation proves safe retry
```

### protocol repair

```text
missing/invalid WorkerResultProposal
```

### manual / no-retry

```text
workspace boundary violation
sandbox widened
credential mutation attempt
unknown provider identity
unreconcilable side effect
```

---

# 34. Files to Add

Expected new production files:

```text
internal/team/execution_world.go
internal/team/execution_world_local.go
internal/team/workspace_snapshot.go
internal/team/workspace_snapshot_git.go
internal/team/process_supervisor.go
internal/team/process_supervisor_unix.go
internal/team/process_supervisor_windows.go

internal/team/subagent_binding.go
internal/team/subagent_external_result.go

internal/team/subagent_codex.go
internal/team/codex_appserver_client.go
internal/team/codex_appserver_protocol.go
internal/team/codex_result_schema.go
internal/team/codex_process.go
```

Test helpers MAY be placed in existing test fixture files instead of creating parallel frameworks.

---

# 35. Existing Files Expected to Change

At minimum inspect/modify:

```text
internal/agent/agent.go
internal/team/parse.go

internal/team/coordinator.go
internal/team/coordinator_execute.go
internal/team/coordinator_task_run.go
internal/team/coordinator_run.go

internal/team/services.go
internal/team/subagent_provider.go
internal/team/subagent_registry.go
internal/team/subagent_hufu.go

internal/team/status.go
internal/team/task_occurrence_projection.go
internal/team/projection_shadow.go

internal/team/event_types.go
internal/team/event_payloads.go
internal/team/event_reducers.go
internal/team/coordinator_eventstore.go

internal/team/execution_receipt.go
internal/team/task_result.go
internal/team/verification.go
internal/team/recovery.go
```

Exact file placement MAY follow current repository organization, but the ownership boundaries in this specification MUST remain.

---

# 36. Implementation Plan

Implement as small reviewable PRs.

---

## Phase 0 — Freeze baseline semantics

### PR-00: External-provider baseline tests

No production behavior changes.

Add tests proving current local provider semantics:

```text
TestLocalSubagentProviderParity
TestProviderCannotAcceptTask
TestUnknownSubagentProviderFailsClosed
TestDurableTaskModelDoesNotRetargetOnResume
```

Record normalized baseline for:

- result;
- receipt;
- retry classification;
- verification;
- completion;
- replay.

**Gate:** all existing tests pass.

---

## Phase 1 — Durable provider binding

### PR-01: Config schema

Implement:

- team provider configs;
- team default;
- agent provider default;
- protected task provider pin.

Do not execute Codex.

Tests:

```text
TestSubagentProviderConfigParsing
TestTaskProviderIsNotCoordinatorJSON
TestProviderResolutionPrecedence
TestReservedLocalProviderCannotBeOverridden
```

### PR-02: Todo/event durable binding

Add:

- Todo provider field;
- `ProviderBinding`;
- event payloads;
- reducer;
- replay/shadow projection;
- occurrence contract comparison.

Tests:

```text
TestProviderBindingRoundTripReplay
TestProviderBindingDoesNotRetargetAfterConfigChange
TestProviderSessionBindingIsIdempotent
TestProviderBindingAppendFailureDoesNotAdvanceProjection
```

### PR-03: Coordinator resolves durable provider

Replace fixed:

```go
Resolve(localSubagentProviderName)
```

with canonical occurrence provider resolution.

Default remains `hufu-local`, therefore user-visible behavior should not change.

This PR MUST also add the direct-fast-path guard from §28.1: `createDirectAgent` MUST reject/escalate (not silently execute) whenever the resolved provider is not `hufu-local`. This guard — not full direct-path parity, which is deferred to PR-15 — is the Phase-1 deliverable for the direct path.

Tests (in addition to those already listed for provider resolution):

```text
TestDirectFastPathFailsClosedForNonLocalProvider
```

**Renamed 2026-09-08**: implemented as `TestDirectFastPathEscalatesNonLocalProviderToCoordinatedDispatch` (`internal/team/subagent_binding_test.go`) — the name changed to match §28.1's actual chosen behavior (escalate to coordinated dispatch, not reject outright), not a change in what the guard proves: a non-`hufu-local` provider can never silently run through the direct fast path.

**Gate:** full local provider parity.

---

## Phase 2 — Untrusted result boundary

### PR-04: `WorkerResultProposal`

Add strict proposal schema and canonicalizer.

Use a fake external provider only.

Tests:

```text
TestExternalProviderCannotSetTaskID
TestExternalProviderCannotForgeReceipt
TestExternalProviderCannotForgeEvidence
TestExternalProviderCannotForgeArtifactHash
TestExternalProviderCannotForgeVerification
TestExternalResultCanonicalUsesActualWorkspaceDelta
TestExternalResultRejectsOutsideWorkspaceArtifact
```

### PR-05: Workspace snapshot/delta

Implement Git + fallback snapshotter.

Tests:

```text
TestWorkspaceDeltaAddedModifiedDeleted
TestWorkspaceSnapshotRejectsSymlinkEscape
TestWorkspaceDeltaHashesActualBytes
TestWorkspaceSnapshotFallbackNonGit
```

**Renamed/split 2026-09-08**: `TestWorkspaceSnapshotRejectsSymlinkEscape`'s literal "reject" behavior was corrected (see §SEC-08's PR-16-era note) to "tolerate a pre-existing escaping symlink (placeholder identity, content never read), still detect a newly appearing one" — a real repository can already contain an escaping symlink unrelated to any attempt, and hard-failing the whole snapshot over it made every attempt against that repository impossible. Implemented as two tests in `internal/team/workspace_snapshot_test.go`: `TestWorkspaceSnapshotToleratesPreexistingSymlinkEscape` and `TestWorkspaceDeltaDetectsNewSymlinkEscape`.

---

## Phase 3 — ExecutionWorld

### PR-06: Generic execution-world contract

Add:

- `ExecutionWorld`;
- local-sandbox;
- environment sanitization;
- baseline snapshot;
- side-effect mapping.

Still no Codex.

Tests:

```text
TestExecutionWorldSideEffectMapping
TestExecutionWorldDoesNotInheritSecrets
TestExecutionWorldRejectsCredentialMutation
TestExecutionWorldRejectsOutsideWritableRoot
```

### PR-07: Process supervisor

Implement reliable process-tree cleanup.

Tests use helper child processes, not Codex.

```text
TestProcessSupervisorKillsDescendants
TestProcessSupervisorCancellation
TestProcessSupervisorTimeout
```

If platform cannot satisfy the test, external provider preflight must report unsupported.

---

## Phase 4 — Codex app-server transport

### PR-08: JSON-RPC client

Implement:

- stdio framing;
- request IDs;
- concurrent notification dispatch;
- bounded frames;
- startup timeout;
- initialize;
- terminal detection.

Use a fake app-server subprocess fixture.

Tests:

```text
TestCodexRPCInitialize
TestCodexRPCUnknownNotificationIgnored
TestCodexRPCOversizedFrameFails
TestCodexRPCProcessCrashFailsInflightRequests
TestCodexRPCCancellation
```

### PR-09: Thread lifecycle

Implement:

```text
thread/start
thread/resume
turn/start
turn/interrupt
thread/read
```

Fake server first.

Tests:

```text
TestCodexNewThreadPersistsBindingBeforeTurn
TestCodexResumeUsesDurableThreadID
TestCodexEffectiveModelMismatchFails
TestCodexSandboxMismatchFails
TestCodexCWDWideningFails
```

### PR-10: Structured result

Implement Codex output schema and final `thread/read` extraction.

Tests:

```text
TestCodexOutputSchemaMatchesWorkerResultProposal
TestCodexMissingProposalIsProtocolIncomplete
TestCodexInvalidProposalDoesNotBecomeTaskResult
TestCodexTerminalReadWinsOverDroppedActivityNotification
```

---

## Phase 5 — Real Codex provider

### PR-11: `CodexSubagentProvider`

Register configured Codex provider.

Production flow:

```text
Prepare execution world
→ snapshot baseline
→ start app-server
→ initialize
→ start/resume thread
→ persist session binding
→ turn/start
→ events/status
→ turn/completed
→ thread/read
→ result proposal
→ final snapshot
→ canonicalize
→ return AttemptResult
```

Do not move retry/verification into the provider.

### PR-12: Cancellation / crash / transcript

Add:

- interrupt;
- process-tree cleanup;
- partial evidence;
- transcript artifact;
- provider failure classification.

Tests:

```text
TestCodexCancellationPreservesBindingAndTranscript
TestCodexCrashPreservesWorkspaceDelta
TestCodexTimeoutDoesNotReportSuccess
```

---

## Phase 6 — Protocol repair / resume

### PR-13: Result-only repair

Implement a read-only schema repair turn.

Tests:

```text
TestCodexResultRepairIsReadOnly
TestCodexResultRepairDoesNotReplayWorkspaceWrite
TestCodexSecondInvalidResultBecomesProtocolIncomplete
```

### PR-14: Crash/restart reconciliation

Tests:

```text
TestCodexResumeAfterHufuRestart
TestCrashAfterProviderSessionBeforeTurnTerminal
TestCrashAfterWorkspaceMutationBeforeVerification
TestResumeDoesNotReplayUnsafeSideEffect
```

---

## Phase 7 — Fast path and compatibility

### PR-15: Direct-agent parity

Either fully support external providers in direct path or force those agents through coordinated execution.

Test:

```text
TestDirectCodexWorkerPreservesNormalTaskSemantics
```

### PR-16: Reports/TUI/debug bundle

Expose provider identity without leaking secrets.

Test:

```text
TestReportShowsProviderIdentity
TestDebugBundleRedactsProviderTranscript
```

**Renamed 2026-09-08**: `TestReportShowsProviderIdentity` is implemented as `TestView_DetailShowsNonLocalProviderIdentity` (`internal/tui/l2_view_test.go`) — the TUI detail view's `providerLine` shows only the Hufu-assigned provider name (never anything provider-issued, like a session id or transcript, that could carry a secret), and only when it differs from the `hufu-local` default. `TestDebugBundleRedactsProviderTranscript` is implemented under the same name's `cmd/hufu` convention as `TestDebugCmd_RedactsProviderTranscript` (`cmd/hufu/debugcmd_test.go`), whose own doc comment cross-references this spec entry directly.

---

# 37. Mandatory Integration Test Matrix

The feature is NOT complete until these pass.

| Test | Required proof |
|---|---|
| `TestLocalSubagentProviderParity` | local behavior unchanged |
| `TestProviderResolutionPrecedence` | task > agent > team > local |
| `TestDirectFastPathFailsClosedForNonLocalProvider` (implemented as `TestDirectFastPathEscalatesNonLocalProviderToCoordinatedDispatch`, see §36 PR-03) | direct path cannot silently bypass provider binding before PR-15 parity lands |
| `TestProviderBindingRoundTripReplay` | provider survives restart |
| `TestProviderCannotAcceptTask` | provider success does not bypass verification |
| `TestExternalProviderCannotForgeReceipt` | receipt authority remains Hufu |
| `TestExternalProviderCannotForgeEvidence` | HMAC/evidence authority remains Hufu |
| `TestExternalProviderCannotForgeArtifactHash` | hash is computed from bytes |
| `TestExternalResultCanonicalUsesActualWorkspaceDelta` | filesystem truth wins |
| `TestExecutionWorldDoesNotInheritSecrets` | env is allowlisted |
| `TestExecutionWorldRejectsOutsideWritableRoot` | root boundary enforced |
| `TestProcessSupervisorKillsDescendants` | no orphan coding processes |
| `TestCodexNewThreadPersistsBindingBeforeTurn` | durable resume identity |
| `TestCodexResumeUsesDurableThreadID` | retry/restart reuses thread |
| `TestCodexSandboxMismatchFails` | no permission widening |
| `TestCodexEffectiveModelMismatchFails` | no model substitution |
| `TestCodexMissingProposalIsProtocolIncomplete` | no free-text success shortcut |
| `TestCodexCancellationPreservesBindingAndTranscript` | partial evidence retained |
| `TestCodexCrashPreservesWorkspaceDelta` | crash does not erase effects |
| `TestCodexResultRepairIsReadOnly` | repair cannot mutate again |
| `TestResumeDoesNotReplayUnsafeSideEffect` | recovery remains safe |
| `TestFailedCodexVerificationRetriesThroughHufu` | retry authority is Hufu |
| `TestCodexSuccessStillNeedsCompletionGate` | run acceptance remains Hufu |

---

# 38. Real Codex Smoke Tests

Unit/integration CI MUST NOT require a live Codex account.

Provide opt-in smoke tests:

```bash
HUFU_CODEX_SMOKE=1 go test ./internal/team -run CodexSmoke
```

Smoke prerequisites:

```text
codex installed
CODEX_HOME pre-authenticated
explicit scratch repository
network policy declared
```

Smoke tests MUST use a temporary repository and MUST NOT modify the Hufu source checkout.

Required smoke scenarios:

1. read-only inspect task;
2. workspace-write task creating one file;
3. compile/test task;
4. cancellation;
5. retry using same thread;
6. restart app-server then `thread/resume`;
7. invalid artifact proposal rejected by Hufu.

**Status (2026-09-08, updated again — now run live end to end):** this formal `HUFU_CODEX_SMOKE=1` suite exists (`internal/team/codex_smoke_test.go`, `TestCodexSmoke*`), covering all seven required scenarios above by driving the real production `CodexSubagentProvider.RunAttempt` path against the actually installed `codex` binary in a throwaway scratch git repository — never the Hufu source checkout. It is gated on `HUFU_CODEX_SMOKE=1` plus a live `codex` on `PATH` and a pre-authenticated `CODEX_HOME` (`codexSmokeRequireReady`); absent any of those, every scenario `t.Skip`s with a clear reason. The target model is configurable via `HUFU_CODEX_SMOKE_MODEL` (default `gpt-5-codex`), added after the first live run showed no single model name works across accounts — a ChatGPT-plan login rejected `gpt-5-codex` outright ("not supported when using Codex with a ChatGPT account") while accepting the account's own configured default instead.

The suite was then actually run live against a real, authenticated (ChatGPT-plan) Codex account, and — exactly the value a live run over a fake-server-only suite provides — it surfaced two genuine defects no existing test could reach, because both require conditions no fake-server harness's workspace ever has:

- **CODEX_HOME preflight over-rejection**: §25's preflight (`codexHomeLooksAuthenticated`) treated an unset `CODEX_HOME` env var as an outright failure, when the real codex CLI itself defaults to `$HOME/.codex`. Every one of the first live run's seven scenarios failed preflight with "CODEX_HOME is not set" even though a real, authenticated `CODEX_HOME` existed at exactly that default location. Fixed by applying the same `$HOME/.codex` fallback preflight itself now uses; regression-tested by `TestCodexHomeLooksAuthenticatedFallsBackToDefaultWhenUnset`/`...FallbackFailsWithoutAuthFile` (`internal/team/subagent_codex_test.go`).
- **Git-assisted workspace snapshot missed Hufu's own bookkeeping** (§11.3/§11.4): `gitCandidateFiles` (the git-optimized snapshot path) had no exclusion for Hufu-internal directories (`logs/`, `.git`, etc.) — only the non-git fallback walk did. Every fake-server unit test's workspace is a plain non-git temp dir, so this path was never exercised by the whole existing suite. Against a real git-initialized scratch repository, Hufu's own `logs/event_store.jsonl` became a legitimate git "untracked file" snapshot candidate, and any append the Coordinator's own `EventStore` made to it during a real attempt showed up as an external `Modified` delta entry — indistinguishable from a provider write, and fatal to a read-only task (empty `WritableRoots`, so *any* observed change fails closed). The second live run's `TestCodexSmokeReadOnlyInspect`/`RetryUsesSameThread`/`RestartThenResume` all failed with exactly this: `provider_workspace_violation: ... "logs/event_store.jsonl" changed outside every authorized writable root`. Fixed by giving `gitCandidateFiles` the same internal-directory exclusion the fallback walk already had (`isWorkspaceInternalPath` in `internal/team/workspace_snapshot.go`); regression-tested by `TestWorkspaceSnapshotGitOptimizedSkipsInternalDirs` (`internal/team/workspace_snapshot_test.go`), reproduced first as a synthetic two-snapshot-around-one-append repro before the general fix.

With both fixed, a third live run passed all seven scenarios end to end (`HUFU_CODEX_SMOKE_MODEL=gpt-5.6-luna`, the account's own supported model): read-only inspect, workspace-write file creation, compile/test, cancellation, same-thread retry, restart-then-resume, and rejection of a proposal claiming a fabricated file — all against the real app-server, real account, real filesystem. Scenario assertions remain deliberately loose on model wording, strict on the protocol/safety properties Hufu's own invariants depend on (error class, workspace delta, session identity, rejection).

Its core premises were separately, independently, manually verified live against the real installed `codex-cli 0.153.4` on 2026-09-08 — a full `initialize` → `thread/start` → `turn/start` (with `outputSchema`) → `turn/completed` round trip, including a real `workspace-write` file creation, confirmed the corrected §13/§14 field shapes end to end. That verification is what §13.3, §13.4, §14.1, §14.2, §14.4, and §14.5 above now describe; `internal/team/codex_appserver_protocol.go` and `internal/team/codex_result_schema.go` implement it.

Separately, driving `.agent-teams/hufu-code-review` (a real team, with `reviewer` pinned to `codex`) through the full `cmd/hufu` CLI — not a unit test — surfaced two real defects invisible to every fake-server-driven unit test, because both only manifest through the real `Coordinator` construction path that test harnesses (which construct `*Coordinator` via struct literal) bypass entirely:

- `Coordinator`'s constructor unconditionally initialized `subagentRegistry` with only the local provider, before ever consulting `session.Config.SubagentProviders` — so a configured external provider was silently never registered outside of tests. Fixed by extracting `newSubagentRegistryFor` as the single construction path, called both by the constructor and by `SubagentRegistry()`'s lazy fallback.
- The workspace snapshotter hard-failed the *entire* baseline/final snapshot the moment it encountered any single symlink resolving outside the workspace root anywhere in a real repository — including one belonging to a completely unrelated local tool, pre-existing before any attempt started. Fixed to record such a symlink with a stable placeholder identity (never reading its actual outside-root target content) instead of aborting the walk; §SEC-08 ("MUST NOT be accepted as result artifacts") is now enforced at the artifact-canonicalization boundary specifically, and a *newly appearing* escaping symlink (the actual attack this exists to catch) still shows up in the delta as before.

The end-to-end run itself did not reach a fully completed state in this environment — a shared, memory-constrained sandbox with several unrelated processes already resident killed the run partway through a genuine multi-item Codex-driven review fan-out (confirmed environmental: sequential retries with a smaller task scope hit the same external memory pressure). It did, however, confirm config wiring, task admission through the durable event-sourced pipeline, and real dispatch into `codex app-server` all work correctly together.

That same run also surfaced a third, more serious defect, since fixed: `coordinator_task_run.go`'s completion-gate checks (`validateSubmittedTaskResult`/`validateCompletedTaskResult`, `HandoffState` marking, `coordinatorTaskOutput`'s formatting) all gated on the literal string `Source == "submitted"`, which an external provider's canonicalized proposal never carries (`ExternalProviderProposalSource`, `"external_provider_proposal"`). A Codex-driven attempt reporting `status: "partial"` (or `"failed"`/`"blocked"`) with no independent `task.VerifySpec` would silently reach `TaskDone` carrying that non-terminal status — exactly the bypass §21/§43 exist to prevent, and exactly what `TestFailedCodexVerificationRetriesThroughHufu`/`TestCodexSuccessStillNeedsCompletionGate` (§37) were written to catch. Fixed via a single `isSubmittedResultSource` helper (`internal/team/task_result.go`) that treats both sources as a genuine structured handoff; `TestCodexPartialStatusStaysIncompleteWithoutVerifySpec` (`internal/team/subagent_codex_e2e_test.go`) reproduces the exact silent-bypass scenario and was confirmed to fail without the fix before being left in place as a permanent regression guard. §37's mandatory matrix is now fully satisfied.

**Closed 2026-09-08**: `hufu-code-review`'s `review-workset` `verify-spec` requires a `/files_read` field that `WorkerResultProposal` (§9.1) originally had no way to populate. Resolved by additively extending `WorkerResultProposal` with a `files_read []string` field (§9.1) — the untrusted list of paths the provider claims to have read, verified against the actually-observed workspace delta and, failing that, the live filesystem (the same two-tier check `proposed_files` already gets) before becoming a canonical `TaskResult.FilesRead` entry; an unverifiable claim is dropped for a non-grounded task and fails the whole attempt closed for a grounded one, exactly like a fabricated `proposed_files` entry already does. See `canonicalFilesRead` in `internal/team/subagent_external_result.go`. `TestCodexReviewWorksetStyleTaskSatisfiesFilesReadVerifySpec` (`internal/team/subagent_codex_e2e_test.go`) drives a `review-workset`-shaped `task_result_assert`/`/files_read` verify-spec through the full Codex dispatch path end to end and confirms it now reaches `TaskDone`.

---

# 39. Example Team

```yaml
name: codex-code-worker

subagent-provider-default: codex

subagent-providers:
  codex:
    type: codex-app-server
    command: [codex, app-server]
    protocol: app-server-v2
    execution-world: local-sandbox
    startup-timeout: 15s
    interrupt-grace: 3s
    shutdown-grace: 3s
    max-event-bytes: 1048576
    max-transcript-bytes: 16777216
    inherit-env: [PATH, CODEX_HOME]

unattended: true
no-net: true

delegation:
  allowed-workers: [coder, reviewer]

tasks:
  - id: implement
    agent: coder
    when-goal-contains: implement
    side_effect: workspace_write
    recovery: retry
    execution:
      requires-result: true
      requires-grounded-result: true
    verify: go test ./...
```

Agent:

```markdown
---
name: coder
role: implementation
subagent-provider: codex
side-effect: workspace_write
recovery: retry
---

Implement the assigned task in the authorized workspace.
```

A reviewer MAY still use `hufu-local`:

```yaml
subagent-provider: hufu-local
```

This proves that provider choice is per durable worker occurrence, not global coordinator mode.

---

# 40. Expected User-facing Behavior

Status output SHOULD look like:

```text
TASK implement-01
worker: coder
runtime: codex
provider-session: bound
sandbox: workspace-write
model: <effective model>
attempt: 1
status: in_progress
```

On completion:

```text
provider proposal: success
workspace: 3 modified, 1 added
verification: go test ./... PASS
task: done
```

If Codex says success but verification fails:

```text
provider proposal: success
verification: FAILED
task: retrying (Hufu policy)
```

The UI MUST NOT say "verified by Codex".

---

# 41. Migration / Compatibility

Default configuration remains:

```text
hufu-local
```

Therefore existing teams with no new fields MUST behave identically.

Existing sessions without `SubagentProvider` SHALL replay as:

```text
hufu-local
```

only when they are legacy occurrences created before this schema.

Once a new-format occurrence contains an explicit provider binding, empty/missing provider state is an error rather than an implicit fallback.

Event schema evolution MUST preserve reading of legacy events.

---

# 42. Rollout Strategy

Recommended rollout:

```text
1. merge durable provider binding with no Codex
2. run local-provider parity in CI
3. merge fake external provider + untrusted result boundary
4. merge ExecutionWorld + process supervisor
5. merge app-server fake protocol tests
6. enable real Codex behind explicit config
7. run opt-in smoke suite
8. use Codex for a read-only Hufu review team
9. use Codex for isolated workspace-write implementation tasks
10. only then consider privileged Hufu MCP/tool brokering
```

Do not enable Codex as the global default immediately.

---

# 43. Definition of Done

The project MAY call "Codex worker support" production-ready only when all are true:

- Codex is selected through `SubagentRegistry`;
- selection is durable per task occurrence;
- existing teams default to `hufu-local`;
- Codex uses app-server JSON-RPC, not terminal screen scraping;
- app-server process is bounded and killable as a tree;
- thread ID survives Hufu restart;
- retry can resume the same thread;
- external final output is decoded only as `WorkerResultProposal`;
- trusted `TaskResult` is constructed by Hufu;
- workspace delta is independently observed;
- artifacts are independently hashed;
- receipt is Hufu-issued;
- verification is Hufu-owned;
- completion remains behind CompletionGate;
- `danger-full-access` is impossible through normal config;
- environment is allowlisted;
- secrets are not persisted in provider events/transcripts;
- cancellation and crash preserve partial evidence;
- result-only repair cannot rewrite workspace;
- current local provider tests show semantic parity;
- all mandatory integration tests pass;
- `go test -race ./...` passes;
- `go vet ./...` passes;
- `go build ./cmd/hufu` passes;
- repository lint policy passes.

---

# 44. Explicit Stop Conditions for Coding Agents

During implementation, STOP and fix the design before proceeding if any of the following becomes necessary:

1. Codex must be trusted to claim verification success.
2. Codex must be trusted to create Hufu receipt IDs.
3. Codex must receive Hufu's evidence/HMAC secret.
4. external provider selection must be made by coordinator free text.
5. retry logic must be duplicated inside `CodexSubagentProvider`.
6. Codex needs `danger-full-access` for normal coding tasks.
7. Hufu must pass `os.Environ()` to make Codex work.
8. crash recovery requires replaying an unknown side effect.
9. provider config changes can retarget an already-admitted Todo.
10. direct fast path creates a second, weaker Codex implementation.
11. thread history is treated as Hufu's canonical task state.
12. streaming notifications are the only source used to recover final Codex output.
13. unknown/malformed output is promoted to success via free-text parsing.
14. a result-repair turn can still write the workspace.
15. a provider failure can transition a task directly to `done`.

These are architecture regressions, not implementation inconveniences.

---

# 45. Future Extensions

After v1 is stable, the architecture permits:

## 45.1 Hufu external tool broker

Expose selected Hufu capabilities to coding agents through MCP/dynamic tools:

```text
GitHub
Kubernetes
cloud
database
issue tracker
```

All effects remain Hufu-authorized and Hufu-receipted.

## 45.2 Stronger ExecutionWorld implementations

```text
bubblewrap
systemd transient scope
container
Firecracker
Kubernetes pod
remote worker VM
```

Provider code should not change.

## 45.3 More coding agents

```text
ClaudeCodeSubagentProvider
OpenCodeSubagentProvider
...
```

A new provider is acceptable only if it can return the same neutral `AttemptResult` and cannot bypass Hufu's canonicalization/verification pipeline.

## 45.4 Provider routing policy

Future routing MAY select among coding providers by capability/cost/reliability, but selection MUST be completed and frozen at task admission before side effects.

This can later reuse Hufu's existing capability-routing ideas, without giving the coordinator free-form provider choice.

---

# 46. Source / Protocol Notes

This specification is based on Hufu's current repository architecture and the Codex app-server v2 protocol as present in the reference commits listed at the top.

Relevant Hufu baseline files include:

```text
internal/team/subagent_provider.go
internal/team/subagent_registry.go
internal/team/subagent_hufu.go
internal/team/services.go
internal/team/coordinator_task_run.go
internal/team/status.go
internal/team/execution_receipt.go
internal/team/task_result.go
docs/hufu-runtime-composability-spec.md
```

Relevant Codex protocol facts at the reference baseline:

- `ThreadStartParams` supports model, cwd, runtime workspace roots, approval policy, sandbox and persistent thread creation.
- `ThreadResumeParams` explicitly recommends resuming by `thread_id` when possible.
- `TurnStartParams` supports cwd, runtime workspace roots, approval policy, sandbox policy, model and `output_schema`.
- `turn/interrupt` exists.
- `turn/completed` is a terminal notification.
- **Corrected 2026-09-08** (see §13.4): the real `turn/completed` notification's own payload already carries the final answer inline (`turn.items`' terminal `agentMessage`/`phase:"final_answer"`) — provider clients should not assume every non-terminal *streaming* item is durable, but Hufu does NOT perform a separate `thread/read` to recover the final result; that call is deprecated for this purpose and never made.
- Codex sandbox modes include `read-only`, `workspace-write` and `danger-full-access`; Hufu v1 forbids the last mode.
- Codex approval policy supports `never`; Hufu v1 uses it to prevent provider-side interactive authorization from becoming an alternate policy authority.

Because the Codex protocol evolves, these protocol details are an adapter contract, not Hufu core domain concepts.

---

# 47. Final Architectural Rule

The implementation is correct only if this remains true:

```text
Hufu decides:
    what work exists,
    who is authorized to execute it,
    what execution boundary applies,
    whether the observed result is valid,
    whether retry/recovery is safe,
    and whether the run is complete.

Codex decides:
    how to perform the authorized coding attempt inside that boundary.
```

That separation is the feature.
