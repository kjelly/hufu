---
name: harden-hufu-runtime
description: Use when modifying Hufu coordinator execution, workflow phases, task/result contracts, tool authorization, persistence, recovery, receipts, evidence, action providers, or their integration tests. Guide impact tracing, runtime contract enforcement, projection updates, failure semantics, and full validation.
---

# Harden Hufu Runtime

Apply this workflow to cross-cutting Hufu runtime changes. Read the repository
root `AGENTS.md` first; this skill supplements its always-on rules and does not
replace package-specific guidance.

## Change Workflow

### 1. Classify the change

Identify the canonical owner before editing. Use the closest existing package
and abstraction rather than adding a parallel mechanism.

| Change area | Primary surfaces to inspect |
| --- | --- |
| Task/result/status | `internal/team/todo*`, `task_result*`, `coordinator_task_run.go` |
| Lifecycle/events | `coordinator_eventstore.go`, `event_store.go`, `event_reducers.go`, `execution_events.go` |
| CLI projections | `cmd/hufu/display.go`, `json_output.go`, `report.go`, `team_runner.go` |
| Tool authorization | `tool_policy_gate.go`, `tool_deny.go`, `delegation_policy.go`, phase capability checks |
| Workflow/actions | `action_provider.go`, `execution_context.go`, `phase.go`, `runtime_workflow.go`, `contract_compile.go` |
| Recovery/persistence | `recovery.go`, `disposition.go`, `anti_thrashing.go`, `event_store.go`, session and evidence/receipt code |

### 2. Trace all execution paths

Before changing coordinator behavior, inspect normal workers, direct-agent
execution, extra-model execution, sidecar tasks, runtime actions, fast-path
routing, unattended mode, dry-run mode, and crash-resume. A behavior fixed in
only one path is not fixed.

### 3. Preserve runtime invariants

- Treat `Coordinator` as a state machine. Runtime state, typed results,
  execution receipts, objective verification, and acceptance gates are
  authoritative; model prose is not.
- Use canonical task transition APIs. Preserve checkpoint, task-journal,
  event-store, and status-projection updates together; do not mutate status or
  result fields directly when a transition API exists.
- Route tool authorization through the central policy gate. Do not bypass
  denied-tool, capability, phase, or unattended checks from a special path.
- Treat closed tool sequences as literal contracts. Preflight the concrete
  worker tool set, preserve slot order, and do not add exploratory calls,
  hidden retries, alternate tools, or post-submit calls. Declare expected
  non-zero exit codes explicitly.
- Route failure classification, retry, repair, reconciliation, and
  anti-thrashing through the existing recovery machinery. Never replay a
  completed side effect during protocol repair.
- Keep Hufu core integration-independent. Put consumer-specific commands,
  schemas, inventory, and paths behind generic workflow or `ActionProvider`
  interfaces; do not add Pilot-specific branches to `internal/`.
- Treat artifact references as opaque/content-addressed IDs. Do not confuse
  artifact references with Todo IDs, checkpoint paths, or arbitrary filesystem
  paths.
- Redact secrets before persisting subprocess output, audit logs, transcripts,
  receipts, reports, or sessions, and preserve existing atomic-write and
  workspace-scope protections.

### 4. Propagate observable behavior

When adding or changing a task, workflow, status, or lifecycle event, update
every relevant consumer: event store and reducer, session persistence, task
journal, CLI display, JSON output, reports, TUI reporter, and tests. Do not
declare a feature complete after updating only the coordinator or only one
output format.

### 5. Preflight before model or side-effect execution

Prefer deterministic validation before starting a worker or action provider:

- validate team/workflow/phase configuration and task references;
- resolve required executables and verifier commands;
- verify capabilities, tool grants, closed-sequence tools, paths, and provider
  registrations;
- reject contradictory contracts before an LLM call or external mutation.

If a contract is invalid, fix the contract or fail with a diagnostic. Do not
make the model discover a static configuration error through a failed action.

### 6. Test the contract boundary

For runtime changes, add or update tests covering the applicable layers:

- parsing, schema, and typed-result validation;
- policy, capability, tool-sequence, and phase enforcement;
- event, receipt, evidence, CLI/JSON/report, and TUI projections;
- failed verification, retry/repair without side-effect replay, and
  anti-thrashing;
- atomic persistence and crash-resume behavior;
- secret redaction and workspace/write-path isolation.

Use existing fixtures and test seams. Do not make Hufu tests depend on a Pilot
checkout, external binary, live provider, or consumer-specific fixture.

### 7. Validate and report

Run each required command separately after the change:

```bash
go test ./...
go vet ./...
golangci-lint run
```

For integration or infrastructure mutation, run only with explicit
authorization and report the actual command, exit code, acceptance result,
verification evidence, and unresolved tasks. Passing unit tests or a model
success message is not evidence that an external workflow completed.

## Stop Conditions

Pause implementation and resolve the design first when:

- the canonical owner or transition API is unclear;
- a new path would bypass policy, receipt, verification, or persistence;
- a repair would replay an external side effect;
- a proposed change requires consumer-specific logic in Hufu core; or
- the test plan cannot distinguish success, partial completion, blocked state,
  and verification failure.
