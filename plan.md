# Decision-Aware Runtime Completion Plan

## Goal

Complete docs/hufu-decision-aware-runtime-spec.md so that each V1 requirement
has a safe production input, deterministic enforcement, durable evidence,
crash-resume behavior, and end-to-end tests. A parsed configuration or a pure
helper test does not constitute completion.

Phase 4 and Phase 5 remain gated by spec §49. This plan prepares their
prerequisites but does not bypass them.

## Invariants

- Reuse the existing scheduler, TaskDef, workflow phases, event store,
  artifact store, task transition APIs, recovery machinery, and central tool
  policy gate. Do not create a second DAG or a parallel task lifecycle.
- Runtime state comes from typed results, receipts, verification, events, and
  content-addressed artifacts. Model prose is not evidence of completion.
- Decision-only task configuration remains json:"-" so a coordinator cannot
  lower rigor, inject options, or alter evidence policy through its payload.
- A team without decision configuration, and a task resolved to off, retain
  their current execution behavior.
- Never replay a completed or possibly-completed side effect. Reconciliation
  and failure disposition remain the canonical recovery path.
- All new persisted types are versioned, atomically written, redacted, and
  readable by compatible older sessions.

## Baseline Gaps to Close

1. Production DecisionRequest lacks RequestContract, contract reference,
   base-rate evidence, artifact evidence, and provenance.
2. Outside-view checks a non-empty digest but does not prove that its artifact
   exists and is authorized.
3. DecisionEngine.Resume requires an in-memory request map, so it cannot
   survive process restart.
4. Replan emits an event and blocks later tools, but does not drive a new
   decision lineage or a canonical scheduler outcome.
5. Commit-gate production calls omit the invoked ToolRecoverySpec, so
   require-rollback cannot accept a real compensate tool.
6. Production checkpoints do not receive material-evidence-change or
   side-effect recovery state.
7. Coordinator and judge finalization modes have no production finalizer.
8. The in-progress assumption work needs canonical events, rebuildable
   projections, invalidation behavior, and end-to-end lifecycle tests.

## Ordered Work

Every stage is a barrier. Do not start a later stage until the focused tests,
regression tests, compatibility checks, and validation gate for the current
stage pass.

### Stage 0 — Stabilize assumption lifecycle work

Primary files: decision_assumptions.go, replan.go, decision_index.go,
coordinator_tools_result.go, coordinator_task_run.go, decisioncmd.go.

1. Complete the working-tree assumption changes as one reviewed unit.
2. Retain exactly three status sources: submitted result, declared
   verification, and explicit operator command.
3. Reject blank, duplicate, undeclared, stale, untrusted, or
   artifact-unresolvable status claims.
4. Append canonical lifecycle events/artifacts; the decision index projects
   latest state but must not be the only source of truth.
5. Make a contradicted critical assumption write durable invalidation/replan
   state without mutating the original DecisionRecord.
6. Update session, task journal, event, CLI JSON, report, and TUI projections
   that surface decision state.

Acceptance:

- All three sources pass production-path tests.
- State survives a process restart and the index can be rebuilt from canonical
  records.
- Original DecisionRecord bytes remain unchanged.
- A checked assumption cannot leave an invalidated task reported as success.

### Stage 1 — Create and persist one request-scoped contract

Primary surfaces: coordinator run entry points, TeamSession/session data,
decision engine/store, CLI/config contract inputs.

1. Create RequestContract once per coordinator request revision, before the
   first structured decision.
2. Capture raw request, objective, success criteria, constraints, and declared
   assumptions without asking an LLM to invent a safety contract.
3. Source success criteria from explicit team/request configuration or existing
   typed contracts; add a typed CLI/config input if existing sources are
   insufficient.
4. Persist the contract as a content-addressed artifact and retain its
   reference/revision in DecisionRecord and the first-round judge context.
5. Make an enabled decision fail closed for a missing or invalid contract;
   remove the current nil-contract bypass.
6. A changed request creates a new contract revision and stales dependent,
   unfinished decisions without altering prior records.

Acceptance:

- Missing objective or success criteria fails before sidecar dispatch.
- All judges receive the same contract reference.
- Contract data reconstructs after restart.
- Off-profile and legacy teams behave unchanged.

### Stage 2 — Complete evidence and outside-view inputs

Primary surfaces: internal/agent configuration schemas, TaskDef parsing,
decision_evidence.go, decision_gates.go, artifact store integration.

1. Add configuration-only typed decision evidence declarations for static
   contracts: artifact refs, base rates, assumptions, and provenance.
2. Add a bounded reference-evidence stage for policies that require it. It may
   emit structured evidence artifacts only; it cannot select an option or
   change sealed evidence.
3. Resolve every BaseRateEvidence source through ArtifactStore in the workspace
   authorization boundary. A non-empty but missing, inaccessible, mismatched,
   or invalid artifact fails as decision_outside_view_missing.
4. Construct the production DecisionRequest with contract reference, facts,
   artifacts, base rates, assumptions, and provenance.
5. Persist the complete sealed DecisionEvidencePacket as a
   content-addressed artifact as well as an event payload.

Acceptance:

- Standard/high-stakes profiles run successfully with valid artifact-backed
  evidence.
- Forged/missing evidence fails before JUDGE.
- Material changes stale prior opinions; non-material changes do not.

### Stage 3 — Make decision execution process-resumable

Primary surfaces: decision_engine.go, decision_store.go, event reducers,
session checkpointing, artifact store, ResumeInterruptedTasks.

1. Persist a versioned DecisionRunEnvelope before the first judge dispatch.
2. Include immutable request inputs, profile/policy snapshot, artifact refs,
   task/run/attempt identity, stage progress, and idempotency keys.
3. Rebuild DecisionEngine.Resume from the envelope and event projection, not
   an in-memory map.
4. Reuse sealed packets, valid opinions, challenges, revisions, and finalized
   records; dispatch only missing work.
5. Wire this behavior into actual interrupted-task recovery using the original
   todo ID and existing recovery disposition.

Acceptance:

- After restart at 2/3 opinions, only the third judge is dispatched.
- Repeat resume is a no-op; a final record is never recomputed.
- Interrupted side effects are classified before any retry.
- Legacy sessions remain readable.

### Stage 4 — Finish finalization and provenance projections

Primary surfaces: decision_engine_stages.go, decision_runners.go,
decision_index.go, decisioncmd.go, report/JSON/TUI projections.

1. Implement aggregate, coordinator, and named judge finalization as distinct
   typed paths.
2. Let non-aggregate finalizers see only sealed evidence, aggregates,
   challenges, and revisions; never raw judge conversations.
3. Require a recorded reason for a finalization override and validate its
   option ID against sealed options.
4. Complete provenance ingestion from retrieval/artifact metadata. Model
   parent declarations stay advisory and do not affect grouping.
5. Present profile, record reference, stale state, finalization mode, warnings,
   and outcome consistently in list/show/JSON/report/TUI views.

Acceptance:

- Each finalization mode has a production integration test.
- Forged overrides fail closed.
- Same-content and same-domain source grouping is deterministic.
- All projections agree with the event/artifact state and redact secrets.

### Stage 5 — Turn checkpoint verdicts into scheduler outcomes

Primary surfaces: decision_discipline.go, stop_policy.go, replan.go,
coordinator_task_run.go, todo transitions, disposition/anti-thrashing.

1. Return a typed checkpoint outcome to the task lifecycle rather than only
   appending an event and denying a later tool call.
2. Map replan policy actions onto established behavior:
   - replan: stale old decision and schedule the original task's controlled
     next attempt with a new decision revision;
   - request_information and needs_human: pause with an explicit request and
     no implicit retry;
   - stop: terminally block with criterion evidence;
   - escalate: use existing escalation only after authorization and budget
     admission succeed.
3. Update Todo, session checkpoint, task journal, run events, receipts,
   CLI/JSON/report/TUI, and notifications through canonical transitions.
4. Populate CheckpointState from canonical values: attempt, per-attempt tool
   calls, budget ledger, no-progress detector, assumptions, material evidence,
   and recovery state.
5. Ensure submit_result cannot finalize a task as successful if that same call
   invalidates its decision.

Acceptance:

- Each action has a deterministic lifecycle test.
- Critical contradiction creates a new lineage or explicit human stop.
- No success projection survives an invalidating submit result.
- Off tasks retain previous behavior.

### Stage 6 — Bind commitment to actual tools and recovery

Primary surfaces: tool_policy_gate.go, tool/action metadata, commit_gate.go,
recovery.go, disposition.go, integration tests.

1. Pass the exact invoked tool/action ToolRecoverySpec into EvaluateCommitGate.
2. Verify require-rollback accepts a real CompensateTool and rejects its
   absence.
3. Evaluate commitment at the true mutation boundary; read-only observation
   must not be mistaken for committed execution.
4. Feed reconcile classifications into CheckpointState.SideEffectState.
5. For unknown side-effect state, reconcile or request human action before
   retry. Never auto-replay.
6. Trace ordinary workers, direct agent calls, subagents, sidecars, runtime
   actions, retries, repair, dry-run, unattended mode, and crash resume.

Acceptance:

- Valid compensate tool passes require-rollback.
- Blocked mutation starts zero tool processes.
- Unknown side-effect state never auto-replays.
- Special execution paths use the same gate and recovery rules.

### Stage 7 — Prove V1 end to end and correct the specification

Primary surfaces: team fixtures, cmd/hufu display/JSON/report/TUI, tests, docs.

1. Build a self-contained fixture with light, standard, and high-stakes
   profiles, options, request contract, artifact-backed base rate,
   verification, reconcile policy, and deterministic fake judge sidecar.
2. Exercise: load team → resolve profile → preflight → evidence sealing →
   independent judgments → aggregate/challenge/revision → finalization →
   index → commit gate → execution/verification → decision resolution.
3. Implement all rows A–S of spec §46, including deterministic bytes,
   redaction, authorization, restart, and non-replay tests.
4. Update the specification status and Definition of Done only after each
   claimed production path has named test coverage.
5. Document authoring, migration, evidence artifacts, assumptions,
   verification references, reconciliation, and decision commands.

Acceptance:

- The Phase 3.5 example in spec §43 passes in a coordinator integration test.
- Every V1 Definition-of-Done claim has a production path and durable evidence.
- No item is marked complete solely because a helper or mock-engine test passes.

## Deferred Work After V1

### Stage 8 — Capability-aware routing

Begin only after V1 is stable, trusted capability evidence exists, and the
authorization/capability relationship is explicitly approved. Preserve:

~~~text
authorization eligibility → eligible candidates → capability ranking
~~~

Capability data must be verified or maintainer-declared and can never grant a
tool or bypass phase, path, unattended, or central policy gates.

### Stage 9 — Outcome learning and calibration

Keep decision index and decision resolve as recording/reporting only until
there are at least 30 resolved decisions with at least 20 verified outcomes.
Audit the data and explicitly approve calibration before introducing Brier
scores, calibration reports, or adaptive judge weighting. Outcome quality must
never rewrite historical decision quality.

## Validation Gate for Every Stage

Run and record each command separately:

~~~bash
mise exec go@1.26.6 -- go test ./...
mise exec go@1.26.6 -- go vet ./...
mise exec golangci-lint -- golangci-lint run
git diff --check
~~~

Also run focused invariant tests, event hash-chain verification whenever
persistence changes, and a compatibility test proving off-profile tasks retain
their prior behavior. A red full-suite test blocks completion until it is fixed
or explicitly isolated with evidence.

## Final Checklist

- [x] Every V1 Definition-of-Done item has a real production path.
      Stage 7 found that seven of the eight decision stage purposes were
      unregistered, so no decision could dispatch a judge on any production
      path. Fixed, and guarded by
      `TestEveryDecisionStagePurposeIsRegistered`. Each item's coverage is
      named in spec §47 and in `TestDecisionV1MatrixIsFullyAttributed`, which
      verifies that every test it cites still exists.
- [x] Contracts, evidence packets, run envelopes, records, events, and
      projections are versioned, durable, redacted, and recoverable.
      Covered by the existing durability suite plus
      `TestDecisionV1StandardProfileFormsCompleteRecord` (the record is read
      back through the index, not the in-memory engine) and
      `TestDecisionV1FormationIsDeterministic` (identical runs seal to the
      same hash).
- [x] Policy decisions are deterministic, reason-coded, and projected
      consistently to task/session/event/CLI/report/TUI state.
      `TestCheckpointSchedulerOutcomesProjectCanonicalTodoState` covers the
      five checkpoint actions; every denial carries a reason code
      (`TestCommitGateRequireRollbackUsesInvokedToolContract`).
- [x] No decision, retry, repair, or resume path replays an ambiguous side
      effect. `TestUnknownSideEffectStateNeverAutoReplays` covers both the
      checkpoint and the repair controller, including the `unknown`
      side-effect class that previously fell through to a plain retry.
- [x] Legacy teams and off-profile tasks are behaviorally compatible.
      `TestDecisionV1OffProfileIsInert`,
      `TestParseTeamYMLWithoutDecisionBlock`,
      `TestDisciplineHooksAreNoOpsWhenUnarmed`.
- [x] Phase 4 and Phase 5 have not started before their stated entry gates.
      Made executable rather than remembered:
      `TestPhase4CapabilityRoutingHasNotStarted`,
      `TestPhase5CalibrationHasNotStarted`,
      `TestOutcomeRecordingDoesNotFeedBackIntoJudgment`.

## Status

Stages 0–7 are complete and committed. `go test ./...` is green, which it was
not when this plan was written — three of the failures predated the branch
and two of those tests had never passed since the commit that introduced
them.

Stages 8 and 9 remain **deliberately not started**. Their entry gates are
unmet and are checked, not assumed:

```text
Stage 8  no trusted capability evidence source exists (CapabilityConfig is
         still Required []string), and the capability/authorization
         relationship has not been explicitly approved
Stage 9  Phase5Entry() reports the gate closed: 0 of 30 resolved decisions
         and 0 of 20 verified outcomes
```

Opening either gate is a human decision, not a coding step.
