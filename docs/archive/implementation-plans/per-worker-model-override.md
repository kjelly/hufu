# Hufu Per-Worker Model Runtime Override Specification

> Status: in implementation — archived 2026-09-23 from local scratch space before implementation began
> Authority: reference
> Target: `kjelly/hufu`
> Baseline inspected: `main` at `9d2dedc06be743fc59616e3682e282096e3700cc` (verified against HEAD `789011523b39280609921c850fba836bb6fe2b63` on 2026-09-23, no drift)
> Date: 2026-09-23 (revised 2026-09-23 after codebase verification pass: fixed the `run.go`→`runcmd.go` facade file, added the §4.5 orchestrator role-check fix, tightened §14/§16 dry-run scope, removed ambiguous optional items in §15.3/§18, fenced off an unrelated pre-existing retry-escalation issue in §32)
> Intended audience: coding agent
> Scope: CLI model override, profile integration, validation, runtime resolution, durable execution semantics, observability, documentation, regression tests

---

## 1. Goal

Add a per-worker runtime model override that lets an operator change one or more Agent Team worker execution targets without editing Git-tracked `*.md` agent definitions or `team.yaml`.

Required CLI syntax:

```bash
hufu @hufu-coding \
  --worker-model sa=codex/gpt-6-luna \
  --worker-model coder=codex/gpt-6-sol \
  --worker-model reviewer=codex/gpt-6-sol \
  "implement feature X"
```

The same capability MUST be usable from a named `hufu.yaml` profile:

```yaml
profiles:
  coding-balanced:
    model: codex/gpt-6-luna
    worker-model: "coder=codex/gpt-6-sol,reviewer=codex/gpt-6-sol,final-sa=codex/gpt-6-sol"
```

Usage:

```bash
hufu @hufu-coding --profile coding-balanced "implement feature X"
```

The feature is a **runtime overlay**. It MUST NOT modify:

- `.agent-teams/<team>/*.md`
- `.agent-teams/<team>/team.yaml`
- repository-local files merely to persist the override
- agent prompts
- worker role definitions
- provider credentials

The feature exists specifically to keep the Git-controlled Agent Team definition stable while allowing operator-local model/cost/performance choices.

---

## 2. Design principles

### 2.1 Agent definitions remain immutable inputs

Git-tracked Markdown and `team.yaml` describe the reusable team contract. Runtime model choice is an operator deployment/runtime concern.

Conceptually:

```text
Git-tracked team definition
        │
        ▼
AgentDef / TeamConfig
        │
        ▼
profile runtime overlay
        │
        ▼
explicit CLI runtime overlay
        │
        ▼
effective worker execution target
        │
        ▼
canonical ExecutionTarget frozen at task admission
```

Do not create:

- `<agent>.local.md`
- `team.local.yaml`
- shadow copies of Agent Team Markdown
- automatic rewrites of frontmatter

### 2.2 Reuse the existing execution-target abstraction

The current `--model` flag already represents a worker **execution target**, not merely an opaque model name. Values such as:

```text
codex/gpt-6-sol
ollama/qwen3.5:27b
lemonade/qwen3.5:27b
```

must follow the same parsing, backend resolution, validation, model-profile and execution-policy paths already used by `--model`.

`--worker-model` MUST NOT introduce a second model/provider parser.

### 2.3 Provider identity and model identity remain separate internally

The implementation may accept the compact operator syntax:

```text
codex/gpt-6-sol
```

but the runtime MUST continue to canonicalize it to the existing typed execution target:

```text
ExecutionTarget{
    Backend: "codex",
    Model:   "gpt-6-sol",
}
```

Do not add a second provider field to the CLI override structure.

### 2.4 Explicit CLI remains the highest-authority operator layer

Existing profile behavior is:

```text
explicit CLI > profile > defaults/config
```

Per-worker overrides must preserve that rule.

Within the same source layer, a specific worker override is more specific than a global worker model.

---

## 3. Existing behavior that must remain intact

The current implementation has these relevant properties:

1. `-m / --model` overrides the execution target for all non-coordinator workers.
2. `--coordinator-model` independently controls the coordinator.
3. `applyCLIModelOverrides()` updates team-level role model configuration.
4. `applyCLIGenerationOverridesToAgents()` applies CLI-supplied generation overrides to loaded agents.
5. CLI generation/model overrides are higher priority than agent Markdown frontmatter and team/global config.
6. Profile values are implemented as named bundles of CLI flag strings in `hufu.yaml`.
7. `applyNamedProfile()` skips a scalar profile flag when the user explicitly set the same CLI flag.
8. The runtime later canonicalizes worker selection into immutable `ExecutionTarget` data.
9. Task execution has durable backend/provider binding and model-profile telemetry.
10. Coordinator and auxiliary model roles MUST NOT inherit worker-specific overrides.

All existing behavior must remain backward compatible when `--worker-model` is absent.

---

# 4. User-facing CLI contract

## 4.1 New flag

Add:

```text
--worker-model <agent>=<execution-target>
```

Recommended Cobra/pflag definition:

```go
StringSliceVar(
    &opts.workerModelOverrides,
    "worker-model",
    nil,
    "Override execution target for specific worker(s), repeatable: agent=model",
)
```

The flag MUST support both repeated and comma-separated forms.

Equivalent:

```bash
hufu @team \
  --worker-model coder=codex/gpt-6-sol \
  --worker-model reviewer=codex/gpt-6-sol \
  "..."
```

and:

```bash
hufu @team \
  --worker-model coder=codex/gpt-6-sol,reviewer=codex/gpt-6-sol \
  "..."
```

The repeated form should be the primary documentation form.

## 4.2 Short flag

Do **not** allocate a short flag.

`-m` remains the global worker execution target.

## 4.3 Meaning

`--worker-model` applies only to workers in the selected Agent Team.

It MUST NOT apply to:

- coordinator
- sidecar
- guard
- judge
- plan reviewer
- memory embedding model

Those retain their existing dedicated configuration paths.

## 4.4 Coordinator rejection

The following MUST fail:

```bash
hufu @team --worker-model coordinator=codex/gpt-6-sol "..."
```

Error should direct the operator to:

```text
--coordinator-model
```

Example:

```text
worker-model override targets coordinator "coordinator"; use --coordinator-model instead
```

Also reject an agent whose resolved role is `coordinator` or `orchestrator`, even if its name is not literally `coordinator`.

## 4.5 Existing gap to fix alongside this feature

The current global `--model` worker-exclusion check is inconsistent with the rest of the codebase:

- `cmd/hufu/model_overrides.go:117` (inside `applyCLIGenerationOverridesToAgents`) only excludes `strings.EqualFold(def.Role, "coordinator")` — it does **not** check `"orchestrator"`.
- `cmd/hufu/execution_target_preflight.go:98` (inside `preflightExecutionTargets`) has the same `"coordinator"`-only check.

Every other role check in the codebase treats `coordinator` and `orchestrator` as equivalent, e.g. `internal/team/coordinator_agents.go:398` (`resolveAgentModel`), `internal/team/team_policy_lint.go:449,587,597`, `internal/team/stm.go:187`, `internal/promotion/analyzer.go:146,172`.

While implementing the `--worker-model` role check in §4.4, also fix these two existing `--model` checks to exclude `"orchestrator"` as well, so `--model` and `--worker-model` agree on what counts as a coordinator/orchestrator. This is a small, self-contained fix with no design ambiguity — same pattern, two call sites.

---

# 5. Profile contract

## 5.1 Preserve the current profile schema

Do not change:

```go
Profiles map[string]map[string]string
```

This feature does not justify a profile schema migration.

Use a comma-separated string value:

```yaml
profiles:
  coding-fast:
    worker-model: "coder=codex/gpt-6-luna,reviewer=codex/gpt-6-luna"

  coding-balanced:
    model: codex/gpt-6-luna
    worker-model: "coder=codex/gpt-6-sol,reviewer=codex/gpt-6-sol,final-sa=codex/gpt-6-sol"

  coding-max:
    model: codex/gpt-6-sol
    worker-model: "coder=codex/gpt-6-sol,reviewer=codex/gpt-6-sol,final-sa=codex/gpt-6-sol"
```

## 5.2 No YAML sequence in this phase

This is out of scope:

```yaml
worker-model:
  - coder=...
  - reviewer=...
```

A future profile schema revision may support typed arrays, but this feature should not widen `Config.Profiles`.

---

# 6. Precedence

Precedence MUST be deterministic.

## 6.1 Effective precedence for a worker

Highest to lowest:

```text
1. explicit CLI --worker-model <agent>=<target>
2. explicit CLI --model <target>
3. selected profile worker-model entry for <agent>
4. selected profile model
5. agent Markdown frontmatter model
6. team-level worker-model
7. ~/.config/hufu/hufu.yaml worker-model
8. legacy model compatibility fallback
```

Existing lower-level resolution rules below layer 4 should remain owned by the current loader/runtime code. Do not duplicate them in the override parser.

## 6.2 Important examples

### Example A: profile-specific worker wins over profile global model

Profile:

```yaml
profiles:
  balanced:
    model: codex/gpt-6-luna
    worker-model: "coder=codex/gpt-6-sol"
```

Effective:

```text
coder    -> codex/gpt-6-sol
reviewer -> codex/gpt-6-luna
verifier -> codex/gpt-6-luna
```

### Example B: explicit CLI global model overrides profile worker exceptions

Profile:

```yaml
profiles:
  balanced:
    model: codex/gpt-6-luna
    worker-model: "coder=codex/gpt-6-sol"
```

Command:

```bash
hufu @team --profile balanced -m codex/gpt-6-sol "..."
```

Effective:

```text
all workers -> codex/gpt-6-sol
```

This preserves the existing invariant:

```text
explicit CLI > profile
```

### Example C: explicit CLI worker override beats explicit CLI global model

```bash
hufu @team \
  -m codex/gpt-6-luna \
  --worker-model coder=codex/gpt-6-sol \
  "..."
```

Effective:

```text
coder         -> codex/gpt-6-sol
other workers -> codex/gpt-6-luna
```

### Example D: CLI worker entries merge over profile worker entries

Profile:

```yaml
profiles:
  balanced:
    worker-model: "sa=codex/gpt-6-luna,coder=codex/gpt-6-luna,reviewer=codex/gpt-6-sol"
```

Command:

```bash
hufu @team \
  --profile balanced \
  --worker-model coder=codex/gpt-6-sol \
  "..."
```

Effective keyed override map:

```text
sa       -> codex/gpt-6-luna   # profile
coder    -> codex/gpt-6-sol    # explicit CLI
reviewer -> codex/gpt-6-sol    # profile
```

---

# 7. Profile merge implementation

The current generic profile mechanism treats each flag atomically:

```text
if CLI changed flag -> skip profile flag entirely
```

That is insufficient for a keyed repeatable flag because:

```text
profile:
  worker-model = sa=A,coder=A,reviewer=B

CLI:
  --worker-model coder=C
```

must preserve `sa=A` and `reviewer=B`.

## 7.1 Special-case only `worker-model`

Do not generalize the entire profile system.

`applyNamedProfile()` SHOULD special-case the `worker-model` key.

Pseudo-flow:

```go
explicitGlobalModel := flagWasExplicitlySetByCLI("model")
explicitWorkerModels := copy(opts.workerModelOverrides)

for each profile key:
    switch key:
    case "worker-model":
        if explicitGlobalModel {
            // Explicit global CLI --model outranks all profile worker overrides.
            continue
        }

        profileEntries := parse profile["worker-model"]
        cliEntries := parse explicitWorkerModels

        merged := mergeByAgent(
            profileEntries,
            cliEntries, // CLI overwrites same key
        )

        opts.workerModelOverrides = canonicalize(merged)

    default:
        existing profile behavior
```

Important: capture whether `--model` was explicitly provided **before** profile values are applied, because `pflag.Set()` marks the flag as changed.

The implementation may use a small helper/struct to preserve this source information; do not infer it later from `flag.Changed` after profile application.

## 7.2 Why explicit CLI `--model` suppresses profile `worker-model`

This preserves:

```text
explicit CLI > profile
```

Without this rule:

```bash
--profile balanced -m codex/gpt-6-sol
```

could unexpectedly leave one profile worker on Luna, making `-m` no longer mean “use this worker target for this run”.

---

# 8. Parsing

Add a single parser shared by CLI/profile processing.

Suggested API:

```go
type WorkerModelOverride struct {
    Agent  string
    Target string
}

func parseWorkerModelOverrides(values []string) ([]WorkerModelOverride, error)
```

or a keyed result:

```go
func parseWorkerModelOverrides(values []string) (map[string]string, error)
```

If preserving order helps diagnostics, parse to a slice first and reduce afterward.

## 8.1 Syntax

Split each item using:

```go
strings.SplitN(value, "=", 2)
```

This permits future execution target strings containing `=` after the first separator.

Valid:

```text
coder=codex/gpt-6-sol
reviewer=ollama/qwen3.5:27b
```

Invalid:

```text
coder
=codex/gpt-6-sol
coder=
 coder =
```

Trim whitespace surrounding:

- agent name
- execution target

Do not silently remove whitespace inside the model/target token.

## 8.2 Duplicate agent entries

Within one source layer, the **last occurrence wins**.

Example:

```bash
--worker-model coder=A \
--worker-model coder=B
```

resolves to:

```text
coder=B
```

Reason: this matches common CLI override behavior and makes shell-generated command composition practical.

When profile and CLI both contain the same agent:

```text
CLI wins
```

## 8.3 Canonical agent key

Worker matching SHOULD be case-insensitive for operator ergonomics, while preserving the team’s canonical display name.

Recommended canonical lookup:

```go
strings.ToLower(strings.TrimSpace(agentName))
```

Requirements:

- if exactly one loaded agent matches case-insensitively, use it;
- if no agent matches, fail;
- if the team somehow contains multiple names that collide case-insensitively, fail rather than pick one.

Do not create a new alias system.

---

# 9. Validation

Validation MUST happen after the team is loaded and before model profile warming, coordinator construction, task admission, or provider execution.

## 9.1 Unknown worker

Command:

```bash
hufu @hufu-coding --worker-model codre=codex/gpt-6-sol "..."
```

MUST fail before any model call:

```text
unknown worker "codre" in --worker-model for team "hufu-coding"
available workers: sa, coder, verifier, reviewer, final-sa
```

The exact formatting may follow existing Hufu error style.

## 9.2 Coordinator/orchestrator

Reject as described in §4.4.

## 9.3 Empty target

Reject:

```text
coder=
```

## 9.4 Malformed entry

Reject:

```text
coder
```

## 9.5 Execution target validation

Do not build a new model-name validation system.

After applying the override to the effective agent definition, let the existing canonical execution-selector / static preflight path validate:

- backend existence
- backend kind
- model target syntax
- provider availability
- capability requirements
- execution policy

An override MUST NOT bypass `ExecutionRegistry`, capability validation or model admission.

---

# 10. Runtime data model

## 10.1 `runOptions`

Add:

```go
workerModelOverrides []string
```

This represents explicit CLI values before profile merging.

If implementation needs to preserve profile values separately during profile application, add a runtime-only field such as:

```go
profileWorkerModelOverrides []string
```

Prefer the smallest representation that preserves precedence unambiguously.

Do not add per-worker model configuration to persisted `agent.TeamConfig` merely to carry a CLI overlay.

## 10.2 `ModelCLIOverrides`

Extend the model override snapshot with resolved per-worker keyed overrides.

Recommended:

```go
type ModelCLIOverrides struct {
    Model             string
    WorkerModels      map[string]string
    CoordinatorModel  string
    ContextWindow     int
    Temperature       string
    MaxTokens         string
    TopP              string
    TopK              string
    ReasoningEffort   string
    SidecarModel      string
    GuardModel        string
    JudgeModel        string
    PlanReviewerModel string
}
```

`WorkerModels` keys should already be normalized by the time the runtime applies them.

If syntax parsing needs to remain error-returning, change:

```go
currentModelOverrides()
```

to an error-returning form, or parse the worker map in a dedicated preflight step.

Do not silently drop malformed values.

---

# 11. Applying the override

Current `applyCLIGenerationOverridesToAgents()` is the natural application boundary because it already applies high-priority model/generation settings to loaded `AgentDef`s.

Change it to be able to report validation errors.

Recommended signature:

```go
func applyCLIGenerationOverridesToAgents(
    session *team.TeamSession,
    overrides ModelCLIOverrides,
) error
```

## 11.1 Effective worker model algorithm

For every loaded agent:

```go
if coordinator/orchestrator:
    never apply --model or --worker-model

if specific worker override exists:
    def.Generation.Model = specific target
else if global --model/profile model exists:
    def.Generation.Model = global worker target

apply existing CLI sampling overrides
```

However, §6 source precedence MUST be honored before this point, especially:

```text
explicit CLI --model > profile worker-model
```

Do not attempt to reconstruct profile origin here.

## 11.2 Do not mutate prompts

Only mutate runtime configuration fields.

Do not edit:

```go
def.System
```

and do not serialize the effective override back to disk.

## 11.3 Do not mutate coordinator model

Even if a worker and coordinator share the same underlying model, coordinator configuration remains independent.

---

# 12. Execution backend semantics

`--worker-model` is an **execution-target override**, matching `--model`.

## 12.1 Explicit backend selector

Example:

```bash
--worker-model coder=codex/gpt-6-sol
```

must resolve through the same existing canonical path as:

```bash
--model codex/gpt-6-sol
```

The resulting task execution target must be equivalent to:

```text
backend = codex
model   = gpt-6-sol
```

Do not persist the compact selector as an incorrectly nested model such as:

```text
backend = codex
model   = codex/gpt-6-sol
```

## 12.2 Existing `subagent-provider` interaction

A worker may have:

```yaml
subagent-provider: codex
```

in Markdown.

The implementation MUST reuse the existing `--model` execution-target resolution rules rather than independently combining:

```text
def.SubagentProvider + def.Generation.Model
```

If an explicit override includes a backend selector, the canonical execution-target parser is authoritative. Verified: `Coordinator.resolveCanonicalTaskTarget` (`internal/team/decision_admission.go:314-345`) and `preflightExecutionTargets` (`cmd/hufu/execution_target_preflight.go`) already implement this correctly today — a qualified selector like `codex/gpt-6-sol` skips the `SubagentProvider` fallback branch entirely, so setting `def.Generation.Model` to the compact selector string (as `--worker-model` should do) will not double-qualify. No fix is needed in these two functions; just reuse them unchanged and add the regression test below to prove it.

Add regression coverage for workers whose frontmatter provider and override target differ.

Do not introduce a second provider precedence hierarchy specifically for `--worker-model`.

---

# 13. Durable task / resume semantics

This feature MUST preserve Hufu’s durable task identity and replay behavior.

## 13.1 Before task admission

`--worker-model` changes the effective worker definition used to derive the execution target for **new task occurrences**.

Once the runtime has admitted a task and frozen its canonical:

```text
ExecutionTarget
ExecutionTopology
BackendBinding / ProviderBinding
```

those durable task fields remain authoritative.

## 13.2 Retry of an already admitted task

An in-run retry MUST NOT re-read a newly changed profile or CLI overlay and silently change the task’s frozen execution backend/model.

Retry behavior continues to use the admitted/frozen execution target unless an existing explicit Hufu escalation/replanning mechanism changes it according to its own durable protocol.

## 13.3 Resume

On resume:

- existing persisted task occurrences retain their canonical execution target;
- `--worker-model` may affect newly created task occurrences after resume;
- it MUST NOT mutate historical task events or bindings;
- it MUST NOT rewrite a persisted provider/backend binding.

This is required for replay determinism.

## 13.4 Fresh run

`--new` / a fresh execution naturally admits new tasks using the new effective worker overrides.

---

# 14. Model profile and capability validation

The effective target created by `--worker-model` MUST participate in the existing:

- model-profile resolution
- provider-bound admission
- context-window determination
- capability validation
- execution backend validation
- concurrency policy
- no-net/provider policy

If startup profile warming enumerates statically reachable worker models, it must see the **effective overridden worker models**, not stale Markdown values.

Example:

```text
Markdown coder model = A
--worker-model coder=B
```

Startup/preflight must validate/warm `B`, not `A`, for coder execution.

Do not create duplicate model-profile telemetry.

---

# 15. Observability

Operators need to verify that the override actually took effect.

## 15.1 Startup diagnostics

`displayTeamHeader` (`cmd/hufu/team_loader.go:214`, called from `loadTeamCommon` at `cmd/hufu/team_setup.go:189`) is the existing startup summary line ("Team: ...", "Agents: ..."). Extend it (or add a line immediately after it) to show per-worker overrides only when at least one is present — do not print anything when `--worker-model` was not used.

Suggested:

```text
Worker models:
  coder: codex/gpt-6-sol
  reviewer: codex/gpt-6-sol
```

Do not spam the output with every worker when no per-worker override was supplied.

A single-line form is also acceptable:

```text
Worker models: coder=codex/gpt-6-sol, reviewer=codex/gpt-6-sol
```

## 15.2 Reports

Do not invent a second telemetry store.

Existing task/report output already has canonical execution target and model-profile telemetry. Ensure tasks created using a per-worker override naturally record/report that effective target.

If report generation currently derives worker model information from immutable task receipts/`ExecutionTarget`, no report schema change is needed.

## 15.3 Source/origin observability

Out of scope for this phase. Do not add per-worker override source labels (`source=cli-worker`, `source=profile-worker`, etc.) and do not add or change any persisted event/schema field to carry them.

Correct effective execution identity (§15.1, §15.2) is mandatory; origin labels are not part of this feature.

---

# 16. `--dry-run`

`--dry-run` should resolve and validate per-worker overrides without executing providers.

Expected behavior:

```bash
hufu @team \
  --dry-run \
  --worker-model coder=codex/gpt-6-sol \
  "..."
```

must:

- parse the override;
- verify worker existence;
- reject coordinator targeting;
- resolve effective worker configuration;
- avoid provider execution according to existing dry-run guarantees.

If dry-run displays effective agent/model routing today, include the worker override there.

Note on scope: `--dry-run` short-circuits in `loadTeamCommon` (`cmd/hufu/team_setup.go:225`, returning `team.NewDryRunCoordinator`) *before* `WarmModelProfiles`/`ValidateModelCapabilities` run (`cmd/hufu/team_setup.go:314-319`), and `internal/team/coordinator_dryrun.go`'s target resolution only does string-level canonicalization, not a full `ExecutionRegistry.ResolveTarget`/`ValidateTarget` pass. This is existing behavior and out of scope to change here. Because `applyCLIGenerationOverridesToAgents` (§11) runs *before* the dry-run short-circuit, parsing/worker-existence/coordinator-orchestrator-rejection for `--worker-model` still happen under `--dry-run` as required above — only full model-capability validation (§14) does not, matching today's dry-run behavior for `--model`.

---

# 17. `hufu doctor` / team validation

Do not make team files invalid merely because an operator profile references a different runtime target.

Static `hufu team validate` validates the team definition itself.

Runtime invocation validation validates the selected profile/CLI overlay.

Neither `hufu doctor` (`cmd/hufu/doctor.go`) nor `hufu team validate` (`cmd/hufu/teamcmd.go`) reads `Config.Profiles` today — confirmed by inspection, no `Profiles` reference exists in either file. Keep it that way: do not add profile-worker-name validation to either command. Do not make all profiles globally mandatory/valid for every team because profile worker names are team-specific.

Example:

```yaml
profiles:
  coding:
    worker-model: "final-sa=codex/gpt-6-sol"
```

must not cause unrelated teams without `final-sa` to fail generic startup or config loading.

Unknown worker validation occurs only after selecting both:

- a profile
- a team

---

# 18. Shell completion

Out of scope for this phase. No model-value or `agent=` left-hand-token completion is required or should be implemented.

The flag itself will appear in generated Cobra completion/help automatically as a side effect of registering it with `StringSliceVar`; no additional completion code is needed.

---

# 19. Security

`--worker-model` values are identifiers, not credentials.

Requirements:

- never accept API keys as part of this feature;
- do not log provider credentials;
- do not change `ProviderAPIKey` resolution;
- do not bypass `no-net`;
- do not bypass backend admission;
- do not turn a read-only worker into a write-capable worker;
- do not change `side_effect`, sandbox mode, allowed paths or tool policy.

Changing:

```text
coder -> codex/gpt-6-sol
```

changes execution target only.

It does not change the worker’s authority.

---

# 20. Backward compatibility

With no `--worker-model` and no selected profile containing `worker-model`:

```text
behavior MUST be byte-for-behavior compatible at the configuration level
```

Specifically:

- `--model` behaves exactly as before;
- `--coordinator-model` behaves exactly as before;
- agent frontmatter model precedence remains unchanged below operator overrides;
- team/global `worker-model` behavior remains unchanged;
- profile scalar flag behavior remains unchanged;
- durable task replay remains unchanged.

No migration is required for existing team definitions.

---

# 21. Recommended implementation structure

## Phase 1 — CLI storage and parser

### Modify `cmd/hufu/options.go`

Add:

```go
workerModelOverrides []string
```

### Modify `cmd/hufu/root.go`

Register:

```go
rootCmd.Flags().StringSliceVar(
    &opts.workerModelOverrides,
    "worker-model",
    nil,
    "Override execution target for specific worker(s), repeatable: agent=model",
)
```

### Modify `cmd/hufu/runcmd.go`

This is a separate, required step, not merely "only if needed": the canonical `hufu run` / `hufu decide` facade (`newRunCommand`/`newDecideCommand`/`newCanonicalRunCommandWithOptions`) defines its own `canonicalRunOptions` struct with its own independent flag registrations (e.g. `flags.StringVarP(&options.model, "model", "m", ...)` at `cmd/hufu/runcmd.go:113-124`), which are then copied field-by-field into a `runOptions` value (e.g. `resolved.modelOverride = options.model` at `cmd/hufu/runcmd.go:228`).

Add a matching `workerModelOverrides []string` field to `canonicalRunOptions`, register the same `--worker-model` `StringSliceVar` flag here, and add the corresponding copy line (`resolved.workerModelOverrides = options.workerModelOverrides`) alongside the existing copies. Without this, `hufu run @team --worker-model ...` / `hufu decide @team --worker-model ...` will silently ignore the flag even though `hufu @team --worker-model ...` (the default entry point registered in `root.go`) works.

### Add parser helpers

Preferred location:

```text
cmd/hufu/model_overrides.go
```

or a focused new file:

```text
cmd/hufu/worker_model_overrides.go
```

Keep parsing testable without loading providers.

---

## Phase 2 — Profile keyed merge

### Modify `cmd/hufu/profile.go`

Special-case:

```text
worker-model
```

Implement keyed merge semantics from §7.

Do not redesign all profiles.

Capture whether `--model` was explicitly supplied before applying profile values.

Required behavior:

```text
explicit CLI worker-model > explicit CLI model > profile worker-model > profile model
```

---

## Phase 3 — Loaded-team validation and application

### Modify `cmd/hufu/model_overrides.go`

Extend `ModelCLIOverrides`.

Update:

```go
applyCLIGenerationOverridesToAgents(...)
```

to:

- resolve canonical worker name;
- reject unknown workers;
- reject coordinator/orchestrator (fixing the existing `"coordinator"`-only check per §4.5 so this file's logic matches `resolveAgentModel`);
- apply specific target;
- retain all existing generation override behavior.

Return errors rather than silently ignoring invalid keyed overrides.

### Modify `cmd/hufu/execution_target_preflight.go`

Fix the same `"coordinator"`-only role check (§4.5) at `execution_target_preflight.go:98` to also exclude `"orchestrator"`, so static preflight agrees with the override-application logic above.

### Update call sites

Every call to `applyCLIGenerationOverridesToAgents()` must handle the returned error and fail before execution.

---

## Phase 4 — Canonical execution-target compatibility

Verify that an effective overridden:

```go
def.Generation.Model
```

reaches the exact existing canonical `ExecutionTarget` path used by global `--model`.

No known legacy path forces this incorrectly today (verified per §12.2); this phase is a proof step via the tests below, not expected to require a resolver fix. If a test written here does surface such a path, fix it using the existing canonical target resolver (`resolveCanonicalTaskTarget` / `preflightExecutionTargets`) rather than adding worker-override-specific logic.

This phase is complete only when tests prove:

```text
--worker-model coder=codex/gpt-6-sol
```

produces canonical:

```text
backend=codex
model=gpt-6-sol
```

and not:

```text
backend=codex
model=codex/gpt-6-sol
```

---

## Phase 5 — observability/docs

Update generated and hand-authored docs.

Likely files:

```text
docs/reference/operator-command-reference.md
docs/reference/agent-format.md
README.md                    # only if main model-selection docs live here
```

If `operator-command-reference.md` is generated, update the generator metadata/examples and regenerate rather than hand-editing generated output.

---

# 22. Expected files touched

At minimum inspect/change:

```text
cmd/hufu/options.go
cmd/hufu/root.go
cmd/hufu/runcmd.go                      # canonical `hufu run`/`hufu decide` facade: flag + copy-through (required, see §21 Phase 1)
cmd/hufu/model_overrides.go
cmd/hufu/model_overrides_test.go
cmd/hufu/execution_target_preflight.go  # fix existing coordinator-only role check, §4.5
cmd/hufu/profile.go
cmd/hufu/team_loader.go                 # displayTeamHeader startup diagnostics, §15.1
docs/reference/operator-command-reference.md
docs/reference/agent-format.md
```

Potential new test file:

```text
cmd/hufu/worker_model_overrides_test.go
```

Potential runtime regression tests:

```text
internal/team/...execution target tests...
```

Do not modify Agent Team Markdown files merely to test this feature; tests should build temporary teams/fixtures where possible.

---

# 23. Required unit tests

## 23.1 Parser

Test:

```text
coder=codex/gpt-6-sol
```

Expected:

```text
agent=coder
target=codex/gpt-6-sol
```

Test whitespace:

```text
" coder = codex/gpt-6-sol "
```

Expected trimmed key/value.

Test malformed:

```text
coder
```

Error.

Test missing key:

```text
=codex/gpt-6-sol
```

Error.

Test missing target:

```text
coder=
```

Error.

Test duplicate:

```text
coder=A
coder=B
```

Expected `B`.

Test comma-separated values.

---

# 24. Required precedence tests

Use at least two workers.

## 24.1 No new flag

Agent frontmatter values remain unchanged when no global CLI override is present.

## 24.2 Global CLI model

```text
--model A
```

sets all non-coordinator workers to `A`.

## 24.3 Worker CLI model

```text
--worker-model coder=B
```

sets coder to `B`, leaves reviewer on lower-level resolved model.

## 24.4 Global + worker CLI

```text
--model A
--worker-model coder=B
```

Expected:

```text
coder=B
reviewer=A
```

## 24.5 Profile global + profile worker

Profile:

```text
model=A
worker-model=coder=B
```

Expected:

```text
coder=B
reviewer=A
```

## 24.6 CLI global beats profile worker

Profile:

```text
worker-model=coder=B
```

CLI:

```text
--model=A
```

Expected:

```text
coder=A
```

## 24.7 CLI worker beats profile global

Profile:

```text
model=A
```

CLI:

```text
--worker-model coder=B
```

Expected:

```text
coder=B
reviewer=A
```

## 24.8 CLI/profile keyed merge

Profile:

```text
sa=A,coder=A,reviewer=C
```

CLI:

```text
coder=B
```

Expected:

```text
sa=A
coder=B
reviewer=C
```

---

# 25. Required validation tests

## Unknown agent

Fails before execution.

## Coordinator by name

Fails and mentions `--coordinator-model`.

## Coordinator by role

An agent named something else but role=`coordinator` also fails.

## Orchestrator role

Fails for `--worker-model`. Also add a regression test proving the existing global `--model` flag now correctly excludes an `orchestrator`-role agent too (it does not today — fixed per §4.5).

## Case-insensitive matching

If team contains:

```text
Helper
```

then:

```text
--worker-model helper=A
```

must target `Helper`.

## Ambiguous case collision

Fail closed if two agent definitions would match the same normalized operator key.

---

# 26. Required execution-target tests

These are critical.

## 26.1 Explicit Codex selector

Input:

```text
coder=codex/gpt-6-sol
```

Expected frozen target:

```text
Backend = codex
Model   = gpt-6-sol
```

## 26.2 Explicit local/Ollama selector

Input:

```text
coder=ollama/qwen3.5:27b
```

Expected canonical Ollama execution target.

## 26.3 Existing agent `subagent-provider`

Create a worker with a configured provider and verify that explicit execution-target syntax follows the same canonical precedence as global `--model`.

Do not allow double-qualified model IDs.

## 26.4 Capability validation

A worker whose requirements cannot be satisfied by the overridden model must fail through existing model capability validation.

The override must not bypass requirements.

---

# 27. Required durable/retry/resume tests

## 27.1 Frozen admitted task

1. admit task with worker target `A`;
2. change in-memory/operator override to `B`;
3. retry the same admitted occurrence.

Expected:

```text
task remains on A
```

unless an existing explicit escalation/replan mechanism legitimately changes its frozen topology.

## 27.2 Resume

1. persist a task with canonical target `A`;
2. restart/resume with `--worker-model worker=B`;
3. replay existing task state.

Expected:

```text
historical admitted task remains A
```

## 27.3 New task after resume

A newly admitted task for that worker after resume may use `B`.

## 27.4 Provider/backend binding

Existing binding for admitted target `A` MUST NOT be rewritten to `B`.

---

# 28. Required model-profile telemetry tests

For a newly admitted overridden worker:

```text
Markdown/default target = A
worker override         = B
```

assert telemetry/admission uses `B`.

At minimum verify:

- profile resolution receives effective target B;
- report/task execution target shows B;
- no profile-resolution telemetry is incorrectly emitted for stale A solely because the worker Markdown named A.

Do not weaken existing model-profile telemetry invariants.

---

# 29. Required profile tests

Because current profiles are scalar-string bundles:

```yaml
profiles:
  test:
    worker-model: "coder=A,reviewer=B"
```

must parse correctly.

Also test:

- quoted YAML string;
- comma-separated entries;
- explicit CLI keyed override merges with profile;
- explicit CLI `--model` suppresses profile worker-specific entries;
- malformed profile worker-model produces a clear error when the profile is actually effective;
- malformed profile worker-model may remain ignored when explicit global CLI `--model` fully outranks it, matching existing “explicit flag skips lower profile flag” behavior.

Do not validate team-specific worker names while merely loading global config. Validate them after team selection.

---

# 30. Required CLI integration tests

Commands should parse successfully:

```bash
hufu @team --worker-model coder=A "task"
```

```bash
hufu @team --worker-model coder=A --worker-model reviewer=B "task"
```

```bash
hufu @team --worker-model coder=A,reviewer=B "task"
```

```bash
hufu @team --profile balanced "task"
```

```bash
hufu @team --profile balanced --worker-model coder=C "task"
```

Existing commands without the flag must continue to parse exactly as before.

---

# 31. Documentation examples

Add a concise model-selection section.

Recommended operator examples:

## Change all workers temporarily

```bash
hufu @hufu-coding \
  -m codex/gpt-6-luna \
  "implement feature X"
```

## Change only expensive roles

```bash
hufu @hufu-coding \
  -m codex/gpt-6-luna \
  --worker-model coder=codex/gpt-6-sol \
  --worker-model reviewer=codex/gpt-6-sol \
  --worker-model final-sa=codex/gpt-6-sol \
  "implement feature X"
```

## Save locally in `~/.config/hufu/hufu.yaml`

```yaml
profiles:
  coding-balanced:
    model: codex/gpt-6-luna
    worker-model: "coder=codex/gpt-6-sol,reviewer=codex/gpt-6-sol,final-sa=codex/gpt-6-sol"
```

Run:

```bash
hufu @hufu-coding --profile coding-balanced "implement feature X"
```

This is the primary workflow that avoids touching Git-controlled Agent Team Markdown.

---

# 32. Non-goals

Do not implement in this change:

- per-worker temperature override;
- per-worker max-tokens override;
- per-worker reasoning-effort override;
- per-worker context-window override;
- new typed profile YAML schema;
- `.local.md` overlay files;
- GUI/TUI model editor;
- automatic cost optimizer;
- automatic worker-to-model benchmarking;
- model aliases;
- changing worker authority/tool permissions;
- changing task side-effect classification;
- changing model escalation policy;
- rewriting old durable task events;
- fixing `escalateTaskModelForRetry`'s interaction with an already-frozen `ExecutionTarget` (`internal/team/coordinator_eventstore.go`'s `CommitTaskResetForRetry` only updates the legacy `Model` string, not `ExecutionTarget`/`ExecutionTopology`, for tasks that already have a frozen target). This is a pre-existing, unrelated retry-escalation code path. Do not touch it, and do not treat it as a blocking discovery if noticed while writing the durable retry/resume tests required by §27 — those tests only need to confirm that `--worker-model`/`--model` changes do not retroactively affect an already-admitted task's frozen target, which is already true.

A future generalized flag may look like:

```text
--worker-set coder.model=...
```

but that abstraction is not justified yet.

---

# 33. Acceptance criteria

Implementation is complete only when all statements below are true.

1. `--worker-model agent=model` exists.
2. The flag is repeatable.
3. Comma-separated keyed entries work.
4. Profile `worker-model` works without changing the existing profile schema.
5. Multiple profile worker entries can be expressed in one string.
6. CLI worker entries merge by agent over profile entries.
7. Explicit CLI `--model` overrides profile worker entries.
8. Explicit CLI `--worker-model` overrides explicit CLI `--model` for that worker.
9. Unknown workers fail before provider execution.
10. Coordinator/orchestrator targets are rejected for `--worker-model`, and the pre-existing `--model` coordinator-only (missing orchestrator) gap in `model_overrides.go` and `execution_target_preflight.go` is fixed to match (§4.5).
11. `hufu run`/`hufu decide` (`cmd/hufu/runcmd.go`) accept `--worker-model` identically to the default entry point (`root.go`).
12. The override never changes Git-tracked Agent Team files.
13. The override never changes prompts or role/tool authority.
14. Explicit backend/model selectors use the same canonical execution-target path as `--model`.
15. Existing durable task execution targets remain immutable across retry/resume.
16. New tasks use the effective per-worker target.
17. Model-profile/capability validation sees the effective overridden target (outside `--dry-run`, per §16).
18. Reports/telemetry expose the actual execution target through existing canonical task evidence.
19. Existing `--model`, `--coordinator-model`, profile and frontmatter behavior remains backward compatible.
20. `go test ./...` passes.
21. `go vet ./...` passes.
22. `golangci-lint run` passes.
23. `go build -o /dev/null ./cmd/hufu` passes.

---

# 34. Suggested implementation order for the coding agent

Execute in this order to reduce semantic regression risk:

```text
1. Add parser + parser tests.
2. Add runOptions field, root flag, and runcmd.go canonicalRunOptions field + flag + copy-through (§21 Phase 1).
3. Fix the existing coordinator-only role check gap in model_overrides.go and execution_target_preflight.go to also exclude orchestrator (§4.5).
4. Add profile keyed merge + precedence tests.
5. Extend ModelCLIOverrides.
6. Apply per-worker overrides to loaded AgentDef with validation.
7. Add exact worker/coordinator/orchestrator/unknown-agent tests (including the §4.5 regression test for --model).
8. Verify/fix canonical ExecutionTarget integration.
9. Add durable retry/resume regression tests.
10. Add model-profile telemetry regression test.
11. Update generated CLI docs/help.
12. Update model precedence documentation.
13. Run full repository validation.
```

Do not start by modifying execution persistence schemas. This feature should fit above the existing durable task boundary.

---

# 35. Completion report required from coding agent

The final implementation report should include:

- files changed;
- exact final precedence implemented;
- CLI examples tested;
- profile example tested;
- whether any existing execution-target code required correction;
- durable retry/resume behavior confirmed;
- tests added;
- `go test ./...` result;
- `go vet ./...` result;
- `golangci-lint run` result;
- `go build -o /dev/null ./cmd/hufu` result;
- any unresolved compatibility issue.

Do not report completion if per-worker override works only by editing Markdown or if resume can silently retarget an already-admitted task.
