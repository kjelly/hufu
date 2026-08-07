# Hufu execution reliability remediation plan

> Status: implemented and verified
> Scope: hufu only (`/home/ubuntu/nfs/github/agent-team-cli`)
> Out of scope: external application workflows and their deployment assets.

## 1. Purpose

This document replaces the original seven-work-package remediation proposal.
That proposal assumed hufu was missing several reliability mechanisms that are
already present in `internal/team`. The remaining work is intentionally narrow:
remove one parallel tool-capability derivation, improve dependency and retry
observability, preserve agent identity consistently in workspace paths, and
prevent contradictory status records.

The governing rule remains:

> A worker's prose claim, a tool transcript, and an observed artifact are
> separate facts. A task only unlocks dependent work after its required
> verifier has passed for the applicable attempt.

This is a code-hardening plan, not a new task-contract rollout. In particular,
it does not add `RequiresVerifiedOutput`, `ResourceAuthority`, `BlockedDetail`,
or a persisted `EffectiveToolset` type.

Implementation completed R1–R6. R4 was delivered as a production-path
crash-resume regression test, as intended; the existing reconciliation
mechanism did not require a second implementation.

## 2. Source-audit correction

The original plan overstated the remaining gap. The current source already
implements most of the proposed controls:

| Original WP | Audited state | Disposition |
|---|---|---|
| WP-1 tool parity | Mostly complete. One parallel derivation remains: `workerExposedToolNames` predicts the worker's tools separately from the final `[]fantasy.AgentTool` assembled for the model, and it omits the agent-specific MCP tools appended later. | Keep only the single-source fix in R1. |
| WP-2 verified dependency | The scheduler already dispatches only when every `depends_on` task is `done`, and verifier failure prevents `done`. `markStranded` loses the useful causal evidence. | Replace only the stranded-task representation in R2. Do not add a new dependency schema. |
| WP-3 attempt-fresh evidence | Complete. `ArtifactExpectation.MustBeFresh` compares artifact modtime with `ExecutionReceipt.StartedAt`; producer metadata includes run/task/attempt/producer identity; verifier results are stored on the attempt receipt; the fresh-verification profile exists. | Remove from backlog. |
| WP-4 blocker/resource authority | The proposed contract is not an enforcement boundary. A user-visible `ResourceAuthority` field cannot prevent a worker from mutating a resource through `bash` unless tool policy enforces it. | Do not implement as proposed. Revisit only with a separate, end-to-end tool-policy design. |
| WP-5 fingerprint-safe retry | Core behaviour is complete. `isRepeatedFailure`, `DecideRecovery`, evidence/recovery-policy gates, and anti-thrashing hypothesis validation already suppress unsafe repeats. | Add only explicit suppression observability in R3. |
| WP-6 protocol/prompt | Complete. Result instructions are emitted only when `submit_result` is granted, require truthful non-success outcomes, and reserve a step for submission. Result-only protocol repair re-verifies before marking a task done. | Remove from backlog. |
| WP-7 status projection | The original diagnosis was incorrect for a killed run: no finalizer can run after process death. Projection itself is derived from canonical Todo/session state, and normal and abort finalizers reconcile it. The current run path also reconciles restored state before `ResumeInterruptedTasks`, while Todo changes trigger reconciliation. | No new projection architecture. Keep a focused crash-resume regression test in R4. |

### 2.1 Evidence limitation

The inspected run workspace execution log contained
only nine records and ended with task 4 still `in_progress`; it had no run-level
outcome event. That evidence describes an interrupted process, not a completed
`partial` run with a lagging final projection. The earlier plan predates that
run, so it may have referred to a different, unavailable execution. The plan
must therefore rely on current code paths and reproducible tests rather than
infer a projection defect from this log.

## 3. Invariants

The remaining changes must preserve these invariants:

1. `TodoItem` and terminal-session state are canonical; status files, reports,
   JSON, notifications, and TUI state are projections.
2. `done` means the task's declared verifier passed. A dependency may launch
   only after every producer is `done`.
3. Every model-visible tool name is derived from the final tool slice handed to
   that invocation and is accepted by its runtime authorization gate.
4. Protocol tools remain phase-scoped: execution gets `submit_result`, planning
   gets `submit_plan`, and unrelated paths do not acquire either accidentally.
5. Repeated failures do not rerun a worker unless existing recovery and
   anti-thrashing policy permits it; suppression is observable separately from
   the underlying failure.
6. One logical agent has one stable filesystem identity. Case differences in
   input must not split its history across directories.
7. A projected failure record must not combine a stale success detail with a
   current failure event.

## 4. Remaining work

### R1: derive worker authorization from the actual tool slice

**Problem:** `workerExposedToolNames` in
`internal/team/coordinator_task_run.go` recomputes what a worker is expected to
see. The actual tool slice is assembled separately in
`createTaskAgentWithResultTool`, including `GetAgentMCPTools`. Tests that compare
the allowlist with `workerExposedToolNames` can therefore validate the same
incorrect prediction instead of the tools actually given to the model.

**Change:** introduce a small helper that extracts non-empty `Info().Name`
values from the final `[]fantasy.AgentTool`. At each worker construction path,
derive model-visible authorization names and prompt grants from that final
slice after global MCP, agent-specific MCP, and phase-specific protocol tools
have been appended. Preserve any additional canonical MCP aliases required by
the transport authorizer, but do not predict model-visible names from YAML or a
second tool-selection call.

No full `EffectiveToolset`/`PromptCapabilities` data model is needed. The
coordinator path already demonstrates the desired pattern by using its actual
core-tool map for exposure filtering and authorization.

**Acceptance:**

- tests inspect the exact tool slice passed to `createGatedAgent` rather than
  calling `workerExposedToolNames` as the oracle;
- always-included, global MCP, agent-specific MCP, `submit_result`, and
  `submit_plan` tools are authorized exactly when present;
- execution, planning, direct-agent, delegated-agent, excluded-tool, and
  force-MCP negative cases remain denied as appropriate; and
- prompt text mentions `stm_write`, writable-memory fallback, and result/plan
  submission only when the same final slice and path policy grant them.

**Likely files:**

- `internal/team/coordinator_task_run.go`
- `internal/team/tool_policy_gate.go`
- `internal/team/coordinator_agents.go`
- `internal/team/coordinator_tools_delegate.go`
- `internal/team/coordinator_tools_allowed_test.go`
- `internal/team/tool_policy_gate_test.go`

### R2: preserve failed-dependency evidence when marking stranded tasks

**Problem:** `dagScheduler.markStranded` currently converts every pending task
whose dependencies cannot complete to `skipped` with a generic string. That
loses the producer identity and verifier evidence even though the dependency
gate itself is already correct.

**Change:** mark the downstream task `blocked`, identify the failed direct
producer task ID, and copy or reference that producer's failed verifier
evidence in the downstream failure detail/event. If multiple direct producers
failed, record them in deterministic task-ID order. Persist the same structured
cause through checkpoint/reload.

Do not add `RequiresVerifiedOutput`. A producer that declares a verifier is
already prevented from reaching `done` when verification fails. Requiring
verifier declarations belongs to existing execution profiles and contract
validation, not to a second dependency-edge type.

**Acceptance:**

- a producer verifier failure prevents dependent worker invocation;
- the dependent becomes `blocked`, not `skipped`;
- its persisted evidence names the producer task and failed verification
  result without inventing a new failure; and
- reload/resume preserves the block and its cause.

**Likely files:**

- `internal/team/dag_scheduler.go`
- `internal/team/failure_event.go`
- focused DAG scheduler and resume tests

### R3: emit explicit retry-suppression observability

**Problem:** repeated-fingerprint, insufficient-evidence, invalid-hypothesis,
and anti-thrashing paths already prevent retries, but reports expose only the
resulting `ReplanRequired`, `NeedsHuman`, or blocked state. Operators cannot
tell whether an attempt failed again or a new attempt was deliberately avoided.

**Change:** whenever recovery policy declines a worker replay specifically
because the existing fingerprint/evidence/hypothesis guards reject it, emit a
`retry_suppressed` event and increment a `retry_suppressed` metric. Include
task ID, failure fingerprint when available, disposition, and a stable reason
code. Do not count cancellation, ordinary retry-budget exhaustion, or an
unrelated terminal failure as suppression.

The event is observability only: it must not introduce another recovery
decision path or change the existing disposition.

**Acceptance:**

- an identical fingerprint produces no second worker execution and exactly one
  suppression event;
- incomplete evidence and a rejected/reused strategy use stable, distinguishable
  reason codes;
- a changed, valid hypothesis that permits a retry does not increment the
  suppression metric; and
- checkpoint, JSON/report metrics, and event replay preserve the count.

**Likely files:**

- `internal/team/disposition.go`
- `internal/team/anti_thrashing.go`
- `internal/team/metrics.go`
- retry-disposition, metrics, and event replay tests

### R4: lock crash-resume status reconciliation with an integration test

**Current behaviour:** `Coordinator.Run` rebuilds status projection after
restoring session state and before `ResumeInterruptedTasks`. `SetSessionData`
also installs an `onChange` callback that checkpoints and reconciles every Todo
transition. Normal completion and abort paths reconcile as well.

**Change:** add a regression test rather than another projection mechanism.
Seed a checkpoint and stale `status/*.yml`, restore it through the production
load/setup/run sequence, and assert that status is reconciled before any
resumed worker is dispatched. Include a killed-run shape with an interrupted
task and no run-level outcome event.

**Acceptance:** stale `working` status cannot survive successful resume/setup;
the test exercises the production load call site, not only
`ReconcileAgentStatusesFromSource` directly; and repeated reconciliation is
idempotent.

**Likely files:**

- `cmd/hufu/team_loader.go` or the production setup integration tests
- `internal/team/status_projection_integration_test.go`
- `internal/team/resume_test.go`

### R5: canonicalize agent identity for workspace task/log paths

**Problem:** evidence has been written under both
`tasks/default/Helper/` and `tasks/default/helper/`. Linux treats these as
different directories, so readers can silently miss part of one agent's
history. Todo creation already normalizes agent names in some paths, while
failure/task logging can retain display-case input.

**Change:** define one filesystem-safe canonical agent key and use it for every
agent-owned task/log/status path. Keep the configured/display name inside file
content. Apply the same canonicalization at both writers and readers; do not
silently migrate or delete existing directories in this change. If legacy
mixed-case directories must be read, merge them deterministically and document
the compatibility rule.

**Acceptance:** `Helper`, `helper`, and other case variants write to and read
from one directory; path traversal protections remain intact; concurrent
writes do not create parallel case variants; and existing case-preserving MCP
authorization behaviour is unchanged.

**Likely files:**

- `internal/team/workspace.go`
- task journal/log readers and writers
- focused Linux filesystem tests using a mixed-case agent definition

### R6: keep status detail and failure event coherent

**Problem:** a projected `status/helper.yml` can contain a success detail such
as "Task completed successfully" together with a current execution
`failure_event`. `ReconcileAgentStatuses` currently selects the first non-empty
detail and the first failure event independently, so fields from different Todo
items can be combined into a contradictory record.

**Change:** select detail and failure evidence from one deterministic governing
Todo item for the projected status. For an error projection, prefer the newest
or otherwise canonically ordered error/blocked/protocol-incomplete item and use
both its detail and failure event; clear success detail when failure evidence is
selected. For an idle projection, do not retain a stale failure event. Preserve
terminal-session detail precedence where manual cleanup requires intervention.

**Acceptance:** a status file never pairs success detail with failure evidence;
a later failure supersedes earlier success text; recovery to idle removes stale
failure state; and serialization remains deterministic and idempotent.

**Likely files:**

- `internal/team/status_projection.go`
- `internal/team/status_projection_test.go`
- status projection integration tests

## 5. Explicit non-goals

This remediation does not:

- add `ResourceAuthority` or `BlockedDetail` fields without an end-to-end tool
  enforcement design;
- infer ownership or mutation permission from task prose;
- add `RequiresVerifiedOutput` to dependency edges;
- reimplement attempt freshness, result protocol repair, failure
  fingerprinting, anti-thrashing, or canonical status projection;
- special-case a service, port, process, command, artifact name, terminal text,
  or external project; or
- treat a process kill as a normally finalized partial run.

## 6. Delivery sequence

| Phase | Work | Exit criteria |
|---|---|---|
| P0 | R1 | Authorization and prompt grants are derived from the actual per-invocation tool slice, including agent-specific MCP tools. |
| P1 | R2, R5, R6 | Dependency blocks retain causal verifier evidence; agent history uses one path identity; status records are internally coherent. |
| P2 | R3, R4 | Suppressed retries are explicitly observable and crash-resume projection behaviour is protected by a production-path test. |

Each item should be independently mergeable. R4 may be merged with no
production-code change if the integration test confirms the current call
sequence.

## 7. Test and validation strategy

Use deterministic fake agents, MCP tools, verifier results, checkpoints, and
terminal-session sources. No real service, port, TUI, or external project is
required.

Required regression coverage:

1. Capture the final `[]fantasy.AgentTool` for every relevant invocation path
   and compare its `Info().Name` values with runtime authorization.
2. Verify agent-specific MCP and phase-specific protocol tools in both positive
   and negative cases.
3. Fail a producer verifier and assert that its dependent is blocked with the
   producer ID and verifier evidence after reload.
4. Repeat a failure fingerprint and assert one worker attempt, one
   `retry_suppressed` event, and the expected reason code.
5. Restore a killed-run checkpoint over stale status files and assert
   reconciliation occurs before resumed dispatch.
6. Write task history through mixed-case agent references and assert one
   canonical directory and complete reads.
7. Project success followed by failure and failure followed by recovery; assert
   detail and failure event never contradict each other.

For every implementation change, run:

```bash
go test ./internal/team/...
go test ./internal/tools/...
go test ./cmd/hufu/...
go vet ./...
golangci-lint run
```

## 8. Completion definition

This remediation is complete when R1-R6 are merged (with R4 permitted to be a
test-only confirmation), all required validation passes, and the focused tests
demonstrate:

- actual model-visible tools and runtime authorization cannot drift;
- a failed dependency is blocked with actionable producer/verifier evidence;
- retry suppression is distinguishable from another failed attempt;
- restored sessions repair stale status before resumed work;
- one agent cannot split its history by case; and
- status detail and structured failure evidence describe the same governing
  state.

The completed WP-3, WP-5 core, WP-6, and status-projection mechanisms are not
part of the new implementation backlog. The rejected WP-4 and WP-7 designs
must not be revived without new evidence and a narrower, enforceable proposal.
