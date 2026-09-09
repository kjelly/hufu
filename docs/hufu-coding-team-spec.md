# Hufu Reliable Coding Agent Team — Implementation Specification

**Status:** Proposed implementation specification
**Target repository:** `github.com/kjelly/hufu`
**Target team:** `.agent-teams/hufu-coding/`
**Primary objective:** replace Codex-native SA/coder/reviewer subagent orchestration with a Hufu-owned, durable, bounded workflow in which Hufu owns lifecycle, retry, recovery, verification, review routing, and final acceptance.
**Baseline inspected:** Hufu `main` as of 2026-09-08, including the Codex `app-server` external `SubagentProvider`, durable provider binding/resume, external-result canonicalization, completion gate, and live Codex smoke tests. Before editing, re-read current HEAD and treat existing implementation as authoritative if it has advanced beyond this document.

---

## 1. Outcome

Implement a reusable coding team with the following logical flow:

```text
User request
    │
    ▼
Hufu coordinator
    │
    ▼
SA analysis (read-only)
    │
    ▼
Coder (Codex app-server leaf worker, workspace-write)
    │
    ▼
Verifier (read-only source + command execution)
    │
    ▼
Reviewer (independent read-only code review)
    │
    ▼
Final SA gate (read-only acceptance against original request)
    │
    ├── accepted ──► RunOutcomeCompleted
    │
    └── semantic rejection ──► coder remediation loop
```

The workflow is successful only when the original user request is satisfied, required verification has passed, review has no unresolved must-fix findings, the final SA gate accepts the result, and Hufu's canonical run evaluator reports `completed` with no unresolved tasks.

The implementation MUST optimize for **completion reliability**, not minimum token cost. It MUST nevertheless prevent unbounded retry/review loops and repeated no-progress work.

---

## 2. Architectural rule

### 2.1 Hufu is the control plane

Hufu, not Codex, owns:

- task admission;
- DAG ordering;
- retry budgets;
- failure classification;
- provider/session binding;
- crash recovery and resume;
- workspace/evidence canonicalization;
- verification state;
- reviewer remediation routing;
- anti-thrashing/no-progress policy;
- final run outcome.

No LLM may decide that a transport failure is equivalent to a code-review rejection. No LLM may mark the whole workflow complete merely by emitting a magic string.

### 2.2 Codex is a leaf execution provider

The Codex worker is an external `SubagentProvider` invoked through `codex app-server`. It performs one Hufu-authorized task occurrence at a time.

Codex MUST NOT become a second orchestration layer. In particular:

- do not use Codex native subagents;
- do not let Codex spawn SA/reviewer/coder children;
- do not depend on Codex `wait_agent`, `send_input`, `close_agent`, or equivalent multi-agent lifecycle for correctness;
- do not nest Hufu inside a Codex native multi-agent tree.

### 2.3 Claims are not evidence

A worker's prose claim, tool transcript, workspace delta, verifier result, reviewer result, and final acceptance are separate facts.

A task reaches `done` only through Hufu's existing canonical completion/verification path. External-provider results remain untrusted until canonicalized by Hufu.

---

## 3. Current Hufu baseline to reuse

Do not duplicate mechanisms that already exist. Before implementing, inspect at least:

- `.agent-teams/hufu-code-review/team.yaml`
- `.agent-teams/hufu-code-review/reviewer.md`
- `.agents/skills/team-builder/SKILL.md`
- `internal/team/subagent_provider.go`
- `internal/team/subagent_binding.go`
- Codex `app-server` provider/process/RPC implementation under `internal/team/`
- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_execute.go`
- `internal/team/dag_scheduler.go`
- `internal/team/failure_classify.go`
- `internal/team/disposition.go`
- `internal/team/run_result.go`
- `internal/team/task_result.go`
- `internal/team/event_store.go`
- resume/replay/provider-binding tests
- the opt-in real Codex smoke suite added on 2026-09-08

Important existing behavior to preserve:

1. `SubagentProvider` is narrower than `Coordinator`; Hufu retains retry/recovery/verification/receipt/completion authority.
2. Provider selection is frozen into the durable task occurrence.
3. Codex thread/session identity is durable and may be resumed after provider-process restart.
4. External result proposals are canonicalized against observed workspace state before Hufu accepts them.
5. Codex-reported success must still pass Hufu completion/verification gates.
6. `dagScheduler` already supports bounded `on_failure` loops that reset an ancestor and its dependent wave.
7. `TaskFailureClass`, recovery disposition, retry suppression, and anti-thrashing already exist. Reuse them rather than creating a coding-specific parallel recovery system.

If a requirement in this specification is already implemented generically, add regression tests and reuse it. Do not add a second implementation.

---

## 4. Target team tree

The implementation MUST produce:

```text
.agent-teams/hufu-coding/
├── team.yaml
├── README.md
├── coordinator.md
├── sa.md
├── coder.md
├── verifier.md
├── reviewer.md
└── final-sa.md
```

Runtime changes and tests may be added under `internal/team/` only when the existing runtime cannot enforce an invariant in this specification.

Do not place credentials, `auth.json`, API keys, copied `CODEX_HOME`, or provider secrets under `.agent-teams/`.

---

## 5. Role contracts

### 5.1 Coordinator

**Purpose:** translate one user coding request into exactly one bounded workflow batch and then let the Hufu runtime own progression.

The coordinator MUST NOT:

- edit files;
- run implementation commands;
- perform code review itself;
- invent a second retry loop in prose;
- stop because a reviewer/provider failed once;
- require strings such as `REVIEW_ACCEPTED` or `FINAL_ACCEPTED`;
- dynamically add unrelated agents after the fixed workflow has started unless Hufu's recovery policy explicitly requests replan/escalation.

The coordinator SHOULD perform one delegation batch with the logical indices defined in §7.

### 5.2 SA

**Side effects:** `none`
**Provider:** `hufu-local` by default
**Tools:** read/search only (`view`, `grep`, `glob`, `ls`; no shell unless a concrete need is demonstrated)
**Reasoning:** high

Responsibilities:

- read the original request and relevant repository instructions;
- inspect the smallest sufficient source/test/config surface;
- identify root cause or implementation architecture;
- distinguish symptoms from root cause;
- define scope and non-goals;
- identify compatibility risks and likely regression surfaces;
- define explicit acceptance criteria;
- define the verification commands/checks the verifier must execute;
- identify which tests must be added or changed;
- return one typed `TaskResult`.

SA `success` means the implementation contract is actionable. It does **not** mean code has been changed.

The SA result should place the following information in structured fields supported by the current `TaskResult` schema where practical, otherwise in bounded `details`:

- root-cause/architecture summary;
- implementation steps;
- scope constraints;
- acceptance criteria;
- verification commands/checks;
- risks/open questions;
- files actually inspected.

### 5.3 Coder

**Side effects:** `workspace_write`
**Provider:** `codex` external `codex-app-server` provider
**Recovery:** retry/resume when safe
**Role:** leaf implementation worker

Responsibilities:

- consume the user request, SA contract, and any remediation feedback supplied by Hufu;
- inspect current workspace state before editing;
- fix the root cause rather than only the visible symptom;
- implement the smallest complete change that satisfies the current contract;
- add/update focused regression tests;
- run focused checks as useful during development;
- never weaken tests or validation merely to make the run green;
- return a canonicalizable result proposal; Hufu remains authoritative for identity, workspace delta, evidence, and task completion.

The coder MUST NOT use Codex native multi-agent/subagent tools.

Coder provider/transport failure is not code-review feedback. Hufu should retry/resume the coder occurrence according to provider/recovery policy before considering broader replanning.

### 5.4 Verifier

**Side effects:** source-code mutation forbidden. Command execution is allowed.
**Provider:** `hufu-local` by default
**Tools:** `view`, `grep`, `glob`, `ls`, `bash` (or the narrowest current review preset that can execute checks)

Responsibilities:

- receive the SA-defined verification plan from the task context;
- run the required commands exactly, plus mandatory project validation discovered from repository instructions such as `AGENTS.md` when applicable;
- do not edit source code or tests;
- distinguish command failure from infrastructure/unavailable-tool failure;
- report exact commands, exit status, bounded relevant output, and what was/was not verified;
- return `success` only if all required checks passed;
- return semantic non-success when the implementation fails a required check;
- return blocked/partial when the verifier could not execute the check for an environmental/infrastructure reason.

For the Hufu repository itself, repository instructions currently require at least the project-prescribed Go validation, including `golangci-lint run`; the verifier must obey the repository's current instructions rather than a stale hard-coded list from this document.

V1 may use a verifier worker because that can be built with current Hufu primitives. If a deterministic verification action-provider is added later, it must replace rather than duplicate the verifier execution path and must feed the same canonical verification evidence into downstream review/final acceptance.

### 5.5 Reviewer

**Side effects:** `none`
**Provider:** `hufu-local` by default for independence from the Codex coder; switching to another external provider is allowed only through configuration, not coordinator choice.
**Tools:** read/search; focused test execution may be allowed only if it cannot mutate source.

Responsibilities:

- review only the current change set and the directly relevant caller/callee/test surface;
- compare implementation against the original request and SA contract;
- check correctness, regression risk, API/behavior compatibility, concurrency/error handling, security boundaries, and missing regression tests as relevant;
- report only actionable findings caused by or exposed by the current change;
- do not demand unrelated cleanup;
- do not modify files;
- return a typed result.

Reviewer result semantics:

- `success`: no unresolved must-fix finding;
- semantic non-success: one or more concrete must-fix findings exist;
- `completed_with_gaps`/blocked/partial: evidence was insufficient or the reviewer could not finish; this is **not** equivalent to a code defect and must not automatically send the coder through a rewrite loop.

Every must-fix finding MUST include:

- file/path and relevant location where possible;
- concrete failure scenario;
- why current behavior is wrong;
- evidence inspected;
- expected remediation/test.

### 5.6 Final SA

**Side effects:** `none`
**Provider:** `hufu-local`
**Tools:** read/search only

The final SA is an acceptance gate, not a second general reviewer.

It MUST inspect:

- original user request;
- SA implementation contract;
- current workspace diff/state;
- verification evidence;
- reviewer result and resolved findings;
- any remaining blocked/partial tasks.

It returns `success` only when the requested outcome is actually satisfied. If code is syntactically correct but the requested behavior remains incomplete, it must reject semantically and provide remediation evidence.

A final-SA provider/transport failure must retry final-SA; it must not cause the coder to rewrite working code.

---

## 6. Provider configuration: Codex must be single-agent

Current Codex configuration defaults multi-agent tools to enabled, and `features.multi_agent_v2` can take precedence over `[agents].enabled`. Therefore the Hufu Codex worker must force **both** off.

The target `team.yaml` provider command should use the highest-precedence supported Codex CLI overrides, for example:

```yaml
subagent-providers:
  codex:
    type: codex-app-server
    command:
      - codex
      - -c
      - "agents.enabled=false"
      - -c
      - "features.multi_agent_v2=false"
      - app-server
    startup-timeout: "30s"
    interrupt-grace: "5s"
    shutdown-grace: "5s"
    inherit-env: [PATH, CODEX_HOME, HOME]
```

Before committing this exact command, test it against the installed Codex version used by Hufu. If Codex changes its CLI/config syntax, adapt to the current schema but preserve the invariant: **effective native multi-agent must be disabled**.

Add a preflight/regression test that proves a Hufu coding worker cannot obtain Codex native multi-agent tools. Do not rely only on the coder system prompt saying "do not spawn agents".

`CODEX_HOME` authentication stays operator-owned. Hufu must never copy or serialize its credentials into team config, event payloads, transcripts, reports, or debug output.

---

## 7. Required workflow batch

The coordinator should submit one bounded batch equivalent to:

```text
index 0: SA_ANALYZE
index 1: CODER_IMPLEMENT       depends_on [0]
index 2: VERIFY_IMPLEMENTATION depends_on [1], semantic failure -> 1
index 3: REVIEW_CODE           depends_on [2], semantic failure -> 1
index 4: FINAL_SA_GATE         depends_on [3], semantic failure -> 1
```

Representative task shape:

```json
[
  {
    "agent": "sa",
    "goal": "SA_ANALYZE: analyze the requested coding change and produce the implementation/verification contract",
    "max_retries": 2
  },
  {
    "agent": "coder",
    "goal": "CODER_IMPLEMENT: implement the requested change using the SA contract and current remediation feedback",
    "depends_on": [0],
    "max_retries": 2
  },
  {
    "agent": "verifier",
    "goal": "VERIFY_IMPLEMENTATION: run the SA-required and repository-mandated verification checks",
    "depends_on": [1],
    "on_failure": 1,
    "max_retries": 4
  },
  {
    "agent": "reviewer",
    "goal": "REVIEW_CODE: independently review the current implementation and regression coverage",
    "depends_on": [2],
    "on_failure": 1,
    "max_retries": 4
  },
  {
    "agent": "final-sa",
    "goal": "FINAL_SA_GATE: decide whether the original request is fully implemented using current evidence",
    "depends_on": [3],
    "on_failure": 1,
    "max_retries": 2
  }
]
```

The final implementation may use static `tasks:` contracts with `when-goal-contains` to freeze side effect, provider, execution, result, verification, and recovery policy while allowing `depends_on`/`on_failure` indices from the coordinator's single batch.

Do not use coordinator prose as the only enforcement mechanism.

### 7.1 Do not enable a phase workflow blindly

Hufu's runtime workflow phases are runtime-owned, but the coding remediation loop intentionally jumps from review/final verification back to coder execution. Do **not** add `workflow: [prepare, audit, execute, verify]` merely for appearance if the current phase engine cannot legally re-enter execute from verify through an `on_failure` reset wave.

V1 should prefer the existing DAG scheduler if it already provides the required bounded back-edge correctly. Add phase workflow only after an integration test proves cross-phase remediation preserves lifecycle invariants. Never create two competing state machines for the same loop.

---

## 8. Static team contract requirements

The target `team.yaml` should be reliability-oriented and minimal. A representative starting point is:

```yaml
name: hufu-coding
description: "Hufu-owned SA -> Codex coder -> verify -> review -> final-SA coding workflow"
max-rounds: 64
max-steps: 96
timeout: 3600
verify-timeout: 1800
max-retries: 2
max-concurrent: 1
workspace: workspace
goal-mode: outcome
execution-profile: fresh-session
unattended: true
max-duration: 21600
max-total-tokens: 0
allow-free-text-results: false

subagent-providers:
  codex:
    type: codex-app-server
    command: [codex, -c, "agents.enabled=false", -c, "features.multi_agent_v2=false", app-server]
    startup-timeout: "30s"
    interrupt-grace: "5s"
    shutdown-grace: "5s"
    inherit-env: [PATH, CODEX_HOME, HOME]

delegation:
  bind-task-goal-contracts: true
  allowed-workers: [sa, coder, verifier, reviewer, final-sa]
  no-redispatch-after-success: [sa]

acceptance:
  mode: blocking
  require-no-unresolved-tasks: true
```

This is a **design skeleton**, not permission to add unsupported fields. Validate every field against current `parse.go`, `agent` config structs, `team-builder` skill, and `hufu team validate`.

The coder agent frontmatter must bind `subagent-provider: codex`; SA/verifier/reviewer/final-SA should inherit `hufu-local` unless the operator explicitly configures a different provider.

Do not hard-code a specific OpenAI model name into the team unless the current provider/account is known to support it. Role/model choice should remain operator-configurable. Recommended operational bias is: strongest reasoning model available for SA/final gate, coding-optimized model for coder, and a model/provider independent from coder for reviewer when possible.

---

## 9. Typed result and handoff rules

### 9.1 No shared-conversation assumption

Every edge must be backed by durable task result/evidence. Do not assume the next worker "remembers" another worker's conversation.

Use existing Hufu mechanisms such as canonical `TaskResult`, task result assertions, `FactRefs`, artifacts, receipts, event replay, and context compiler as appropriate.

### 9.2 SA -> coder/verifier/reviewer/final-SA

The SA result must be available to all downstream tasks as bounded context. Prefer runtime-owned structured substitution (`FactRefs`/task result references) rather than coordinator retyping long prose.

If current `FactRefs` can only substitute `Goal`/`Constraints`, use that capability rather than extending it unnecessarily. Add a generic extension only when there is no current safe way to carry the contract.

### 9.3 Review/final failure -> coder

This is a P0 correctness requirement.

When reviewer or final-SA produces a **semantic** rejection and the DAG `on_failure` loop resets coder, the next coder attempt MUST receive the source failure's canonical remediation evidence.

At minimum the retry context must identify:

```text
source task id
source agent/role
source attempt
failure class
typed result status
summary/details
structured findings, when present
verification evidence relevant to the rejection
```

The context must be bounded, redacted, and durable enough to survive process restart/event replay.

Do not solve this by asking the coordinator model to manually copy reviewer prose into a new task. The runtime should propagate remediation evidence generically for an `on_failure` back-edge.

A suitable generic implementation is an invocation/retry context object derived from the source task's canonical failure/result and injected by the context compiler when the target task is reset. Reuse existing event/failure/receipt structures where possible rather than adding a coding-only store.

---

## 10. Failure classification and routing

This is the central reliability requirement.

### 10.1 Semantic rejection is not transport failure

For DAG back-edges, classify the source failure before deciding whether to reset coder.

Expected policy:

| Failure category | Example | Required action |
|---|---|---|
| verification/semantic rejection | tests fail; reviewer finds concrete regression; final SA finds request incomplete | reset coder + downstream wave, inject remediation evidence |
| execution/provider transient | app-server exits, RPC disconnects | retry/restart the same failing worker; do not reset coder unless coder itself is the failing task |
| protocol | malformed/missing external result after repair budget | retry/repair the same provider occurrence; do not interpret as review rejection |
| timeout/stall | provider stops progressing | cancel/restart/resume same task according to recovery policy |
| environment | compiler/tool missing, auth unavailable | replan/block/needs-human as current recovery policy dictates; do not rewrite code blindly |
| contract/policy | invalid team contract, forbidden side effect, sandbox mismatch | fail closed; no coding retry |
| cancelled | user/system cancellation | end/continue according to canonical cancellation semantics; no remediation loop |
| evidence incomplete | reviewer could not inspect enough evidence | retry reviewer or block with explicit gap; do not reset coder merely because review was unavailable |

Use existing `TaskFailureClass` and recovery disposition machinery. Prefer mapping a genuine semantic verification/review rejection to the existing verification-class path if that accurately represents current Hufu semantics.

### 10.2 Failure-aware `on_failure`

At present, if `dagScheduler` routes every terminal error through `on_failure`, add a **generic**, backward-compatible way to limit back-edge routing to allowed failure classes.

Preferred order:

1. first determine whether an existing typed recovery/phase policy already provides the necessary distinction;
2. if yes, reuse it and add tests;
3. if not, add the smallest generic task policy, e.g. a configuration-owned allowlist such as `on-failure-classes`, preserving legacy behavior for teams that omit it.

For `hufu-coding`, coder-reset back-edges must only activate for semantic verification/review/final-gate rejection, not infrastructure failures.

Do not add string matching on error text at this boundary.

---

## 11. Retry and no-progress policy

The workflow must not stop too early, but it also must not loop forever.

Required behavior:

- SA provider failure: retry SA within its budget; if still unavailable, block/partial rather than starting coder without a plan.
- Coder provider failure: restart/resume the same durable Codex thread where safe.
- Coder semantic/verification failure: retry coder with exact evidence.
- Verifier infrastructure failure: retry verifier; do not rerun coder.
- Verifier test failure: rerun coder + verifier/reviewer/final downstream wave.
- Reviewer infrastructure failure: retry reviewer; do not rerun coder.
- Reviewer must-fix finding: rerun coder + downstream wave.
- Final-SA infrastructure failure: retry final-SA; do not rerun coder.
- Final-SA semantic rejection: rerun coder + full downstream validation.

Suggested semantic remediation budget: up to 4 coder remediation waves for verifier/reviewer findings plus up to 2 final-SA remediation waves, bounded further by current Hufu anti-thrashing/no-progress mechanisms.

If the same normalized failure fingerprint repeats with no meaningful workspace/evidence/criterion progress, Hufu should stop automatic replay according to existing anti-thrashing policy and return `partial`/`blocked` with continuation evidence rather than burning tokens indefinitely.

Do not hard-code "stop after second reviewer rejection" as a workflow rule.

---

## 12. Codex session/restart semantics

For one durable coder task occurrence:

- provider identity is frozen at admission;
- Codex thread/session binding must be persisted before a turn depends on it;
- a provider-process crash may start a new `codex app-server` process and resume the same thread when supported;
- task retry must not silently create a different unrelated coding session unless recovery policy explicitly starts a new occurrence;
- a Hufu process restart must reconstruct the task/provider binding from durable state;
- current workspace state and canonical delta remain authoritative over provider claims.

A reviewer/final-SA remediation reset of the coder should preserve the coder's durable provider session when the occurrence/retry model currently guarantees that. Add a test proving the intended behavior. If current reset semantics intentionally create a new occurrence, document and test that instead; never leave it accidental.

---

## 13. Workspace and side-effect safety

The coding team changes source code, but Hufu control data must not become part of the coder's arbitrary cleanup surface.

Until Hufu has first-class enforced separation between control workspace and subject workspace, the coding team/runtime must at minimum:

- prohibit broad destructive cleanup such as `rm -rf` of workspace/project roots;
- prohibit `git clean -fdx`/equivalent ground-up cleanup unless an explicit safe execution contract exists;
- use the existing Hufu-internal-path source of truth to exclude/protect control logs, checkpoints, event store, receipts, and other runtime-owned state;
- prevent a reviewer/SA/final-SA from writing source;
- fail closed when a task's observed workspace delta violates the authorized side-effect scope;
- preserve current symlink/path escape protections;
- never infer provider success from files under Hufu's own bookkeeping directories.

Do not create a second list of Hufu internal directories if the runtime already has a canonical helper; reuse it.

---

## 14. Agent prompt requirements

### 14.1 `coordinator.md`

The coordinator prompt must say, in substance:

1. create the fixed SA -> coder -> verifier -> reviewer -> final-SA batch once;
2. encode ordering with `depends_on` and remediation with `on_failure`;
3. rely on typed task status and Hufu runtime, not magic strings;
4. do not terminate the run solely because a provider/reviewer call failed;
5. do not add extra agents or parallel implementation branches unless runtime replan policy requests it;
6. call `finish` only after Hufu returns the batch in a terminal state and acceptance can be evaluated.

### 14.2 `sa.md`

Must emphasize root-cause analysis, bounded scope, acceptance criteria, and verification planning. It must not edit.

### 14.3 `coder.md`

Must emphasize implementation and root-cause correction, current remediation evidence, focused tests, no native Codex subagents, and truthful result reporting. It must not claim verification it did not observe.

### 14.4 `verifier.md`

Must execute the required commands, not modify source, distinguish code failure from environment failure, and return exact evidence.

### 14.5 `reviewer.md`

Must only report concrete actionable current-change findings and classify semantic failure separately from evidence gaps. It must not edit.

### 14.6 `final-sa.md`

Must decide completeness against the original request and all evidence, not simply repeat reviewer output. It must not accept unresolved tasks or failed verification.

---

## 15. Model/provider policy

Do not make runtime correctness depend on a particular model name.

Default design:

```text
Coordinator : hufu-local, capable but inexpensive model is sufficient
SA          : hufu-local, strong reasoning
Coder       : codex app-server, coding-optimized model
Verifier    : hufu-local
Reviewer    : hufu-local, preferably independent from coder model/provider
Final SA    : hufu-local, strong reasoning
```

Operational recommendation when available:

- SA/final gate: strongest reasoning model the operator is willing to use;
- coder: coding-optimized Codex model;
- reviewer: different model/provider from coder when feasible to reduce correlated mistakes.

Model selection must remain overridable by current Hufu config/frontmatter/CLI precedence. Do not invent a new model registry for this team.

---

## 16. Implementation phases

### Phase 0 — Baseline audit

Before editing:

1. run current targeted tests for Codex external provider, DAG reset, failure classification, replay/resume, completion gate, and hufu-code-review;
2. run `hufu team validate` on existing bundled teams;
3. inspect whether failure-aware back-edge routing and remediation-context propagation already exist at current HEAD;
4. record what is already implemented versus genuinely missing.

Do not rewrite existing working mechanisms.

### Phase 1 — Team files using current runtime

Create the target team and agent files using only current supported schema.

Acceptance for Phase 1:

- `hufu team validate --team hufu-coding` passes;
- `hufu list hufu-coding` resolves all five workers plus coordinator;
- dry-run shows coder bound to Codex provider and other roles to intended providers;
- Codex command disables native multi-agent;
- no role has broader permissions than required.

### Phase 2 — Failure-aware remediation routing

If missing, implement generic typed filtering for DAG `on_failure` so infrastructure/provider failures retry the source worker while semantic verification/review failures can reset coder.

Do not use message substring matching.

### Phase 3 — Durable remediation context

If missing, implement generic source-failure -> reset-target context propagation for `on_failure` loops. Persist enough identity/evidence to behave identically after event replay/restart.

### Phase 4 — End-to-end coding workflow tests

Implement deterministic fake-provider tests before live smoke tests.

### Phase 5 — Opt-in live Codex smoke

Add one or more `HUFU_CODEX_SMOKE=1` scenarios for the complete coding team. Live tests must remain opt-in and must use a throwaway Git repository, never the Hufu source checkout itself.

---

## 17. Mandatory test matrix

At minimum add tests for all of the following.

### A. Clean path

1. SA succeeds.
2. Codex coder changes a file.
3. verifier passes.
4. reviewer returns clean success.
5. final-SA accepts.
6. run outcome is `completed`.
7. no unresolved task remains.
8. provider/receipt identity is recorded correctly.

### B. Verifier finds a real implementation failure

1. coder returns success proposal;
2. verifier reports required check failure;
3. failure is classified as semantic/verification;
4. DAG resets coder and downstream wave;
5. coder retry receives verifier failure evidence;
6. coder provider session resumes according to intended binding semantics;
7. verifier/reviewer/final-SA rerun;
8. eventual success reaches `completed` only after verification passes.

### C. Reviewer finds a real bug

1. verifier initially passes;
2. reviewer returns one concrete must-fix finding;
3. finding is canonical/durable;
4. coder is reset;
5. coder receives reviewer finding in remediation context;
6. verifier reruns even if previous verification was green;
7. reviewer reruns against new diff;
8. final-SA only runs after clean review.

### D. Reviewer provider/runtime failure

Simulate disconnect/process crash/timeout before reviewer produces a semantic result.

Assert:

- reviewer retries/restarts;
- coder is **not** reset;
- workspace does not change because of the reviewer failure;
- no fake code-review finding is synthesized;
- if retry budget is exhausted, run becomes blocked/partial with reviewer infrastructure evidence, not "code rejected".

### E. Final-SA semantic rejection

Final-SA determines the original request is still incomplete even though verification/review are green.

Assert:

- coder receives final-SA remediation evidence;
- coder + verifier + reviewer + final-SA rerun;
- prior green review is not treated as permanent proof after coder changes again.

### F. Final-SA infrastructure failure

Assert final-SA itself retries and coder is not reset.

### G. Codex crash after workspace side effect

Simulate app-server crash after writing a file but before a completed result.

Assert:

- workspace delta is observed by Hufu;
- recovery follows current side-effect/reconciliation policy;
- no blind duplicate mutation is performed;
- provider process tree is cleaned up;
- durable session binding remains coherent.

### H. Hufu process restart mid-coder

Persist event/checkpoint state, reconstruct coordinator, and resume.

Assert:

- same task occurrence/provider identity is restored;
- provider binding is not retargeted by changed mutable config;
- resume does not skip required verifier/reviewer/final gates.

### I. Native Codex subagents disabled

Prove effective Codex worker configuration cannot expose multi-agent spawn tools. Test both legacy `[agents].enabled` and `features.multi_agent_v2` precedence so enabling one cannot silently override the intended single-agent policy.

### J. Evidence gap is not coder failure

Reviewer returns `completed_with_gaps` or equivalent incomplete evidence state without a semantic must-fix finding.

Assert reviewer is retried/blocked according to evidence policy and coder is not reset merely because review could not finish.

### K. Anti-thrashing

Repeat the same semantic failure with no relevant workspace/evidence progress.

Assert bounded retry suppression/needs-human behavior and no infinite coder-review loop.

### L. Budget exhaustion

Expire wall-clock/token budget during a remediation loop.

Assert Hufu emits a canonical partial/blocked outcome with continuation evidence and never reports completed.

### M. Read-only roles cannot mutate

SA/reviewer/final-SA attempts to write must fail closed. Include direct tool and external-provider/sandbox cases that apply to each provider type.

### N. Hufu control data isolation

Hufu's own event/log/checkpoint writes must not appear as coder-authored workspace changes or cause read-only task failure.

---

## 18. Acceptance criteria for the implementation itself

The coding agent implementing this specification may claim completion only when all applicable criteria below are satisfied.

### Team artifacts

- `.agent-teams/hufu-coding/` exists with all files in §4.
- `team.yaml` uses only supported schema fields.
- coder is configuration-bound to Codex external provider.
- Codex native multi-agent is forcibly disabled.
- SA/reviewer/final-SA are read-only.
- verifier cannot intentionally edit source.
- no magic acceptance strings are required.

### Runtime behavior

- semantic verifier/reviewer/final rejection can loop back to coder;
- provider/transport/reviewer infrastructure failure does not incorrectly reset coder;
- coder retry receives durable remediation evidence from the semantic failing task;
- reset of coder invalidates and reruns all downstream verification/review/final evidence;
- retries are bounded and anti-thrashing remains active;
- restart/replay preserves the same logical decisions and bindings.

### Validation

Run the repository's current required validation. At minimum for current Hufu Go code changes, obey `AGENTS.md`, including:

```bash
go test ./...
go vet ./...
golangci-lint run
go build ./cmd/hufu
```

If current repository instructions are stricter, the stricter current instructions win.

Also run:

```bash
hufu team validate --team hufu-coding
hufu list hufu-coding
hufu --agent-team hufu-coding --dry-run "Implement a small safe test change"
```

Run focused race tests where changed concurrency/process/session code makes them relevant, and retain the existing full-suite quality gates.

### Live Codex test

When a real authenticated Codex environment is available, run the opt-in smoke suite against a temporary scratch repository. Failure to have credentials available must skip clearly; it must not cause CI to fail or lead to copied secrets.

---

## 19. Required observability

Reports/debug evidence should make the workflow diagnosable without reading raw model conversations.

For each task occurrence expose or persist, using existing structures where possible:

- task ID / attempt;
- agent role;
- subagent provider;
- provider session/turn identity where safe;
- failure class;
- recovery disposition;
- retry/remediation source task;
- verification state;
- reviewer/final semantic status;
- workspace before/after identity/delta;
- retry suppression reason;
- final run outcome and stop reason.

Never expose credentials or secret-bearing provider config. Reuse Hufu's generic redaction path for transcript/debug bundles.

---

## 20. Non-goals

Do not expand this work into:

- a new general-purpose CI system;
- a second task/event store;
- a coding-specific provider registry;
- native Codex multi-agent support inside coder;
- automatic Git commit/push/PR creation;
- deployment or infrastructure mutation;
- unrestricted network access;
- arbitrary destructive cleanup;
- model-cost optimization before reliability is measured;
- a new Hufu workflow engine if DAG + existing recovery can enforce the required behavior.

Future coding-agent providers should implement `SubagentProvider` and fit the same Hufu control plane without changing team semantics.

---

## 21. Backward compatibility

Any runtime addition must be generic and backward compatible.

If adding failure-class filtering to `on_failure`:

- existing teams that omit the new policy must retain existing behavior;
- durable event replay for old runs must remain valid;
- schema/version bumps must follow existing migration conventions;
- no existing bundled team may become invalid.

If adding remediation context:

- absence of remediation evidence in legacy events must be handled safely;
- context must be bounded and optional;
- current non-looping tasks must receive unchanged prompts unless the new context is present.

---

## 22. Completion report expected from the coding agent

At the end of implementation, provide a concise report containing:

1. files added/changed;
2. whether any Hufu core/runtime change was necessary and why;
3. exact final team topology;
4. exact failure classes that trigger coder remediation versus same-worker retry/block;
5. how reviewer/final remediation evidence reaches coder;
6. how Codex native multi-agent is disabled and tested;
7. tests added;
8. validation commands and results;
9. live Codex smoke result, if credentials were available;
10. remaining limitations that genuinely block production use.

Do not claim completion if a mandatory test/validation is known to fail. Do not treat a reviewer/provider infrastructure failure as a code defect merely to force the workflow forward.

---

## 23. Reference operational invocation

After implementation, the intended user experience should be approximately:

```bash
hufu --agent-team hufu-coding --report \
  "Implement <requested change>. Find and fix the root cause, add regression tests, and finish only after verification and independent review pass."
```

The user should not need to manually switch between SA, coder, reviewer, or final SA. The Hufu runtime owns that lifecycle.

---

## 24. Core invariant summary

The implementation is correct only if these statements are true:

```text
Hufu owns orchestration.
Codex is a leaf worker.
Provider failure != reviewer rejection.
Reviewer unavailable != code rejected.
Verification failure carries exact evidence back to coder.
Review finding carries exact evidence back to coder.
Final rejection carries exact evidence back to coder.
Coder changes invalidate downstream green evidence.
Only verified + reviewed + final-accepted work can complete.
Retries are durable, bounded, and no-progress-aware.
No magic output token is an acceptance boundary.
```
