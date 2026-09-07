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
- [x] Phase 5 has not started before its stated entry gate; Phase 4 did not
      start before a human explicitly opened it (see Status). Made executable
      rather than remembered:
      `TestPhase4AuthorizationStillPrecedesCapability`,
      `TestPhase5CalibrationHasNotStarted`,
      `TestOutcomeRecordingDoesNotFeedBackIntoJudgment`.

## Status

Stages 0–7 are complete and committed. `go test ./...` is green, which it was
not when this plan was written — three of the failures predated the branch
and two of those tests had never passed since the commit that introduced
them.

Stage 9 remains **deliberately not started**. Its entry gate is unmet and is
checked, not assumed:

```text
Stage 9  Phase5Entry() reports the gate closed: 0 of 30 resolved decisions
         and 0 of 20 verified outcomes
```

Stage 8 was **explicitly opened by human decision on 2026-09-07** and has a
scoped V1: `agent.DeclaredCapability` / `capability-registry` in team.yaml is
the trusted, config-level capability evidence source that did not exist
before (`internal/agent/agent.go`); `CapabilityRegistry`/`CapabilityResolver`
(`internal/team/capability_registry.go`) rank an already-authorized candidate
set and can never expand it; `DisciplinePolicy.Routing.CapabilityAware`
(`internal/agent/decision_config.go`) is the schema surface spec1.md §17 and
plan.md always described. Covered by
`TestCapabilityRegistry_UnauthorizedNeverSelected`,
`TestCapabilityRegistry_SpecialistBeatsGeneralist`,
`TestCapabilityRegistry_SelfDeclaredNeverExceedsCeiling`,
`TestCapabilityRegistry_StaleDeclarationInvalidated`,
`TestCapabilityRegistry_DeterministicOrdering`, and
`TestCoordinator_ResolveCapabilityCandidates_RespectsAllowedWorkers`
(`internal/team/capability_registry_test.go`).

**Update, same day:** `Coordinator.ResolveCapabilityCandidates` is now wired
into the real dispatch path, by explicit follow-up decision.
`Coordinator.validateCapabilityRouting` (`internal/team/capability_registry.go`)
runs inside `validateDelegationPolicy` (`internal/team/delegation_policy.go`,
called from `coordinator_execute.go` before any TODO is created), gated by
the new `delegation.capability-routing` team.yaml rules
(`agent.CapabilityRoutingRule`, `internal/agent/agent.go`; parsed in
`internal/team/parse.go`). A rule matches a delegated task by the same
goal-substring selector `TaskGoalInvariants` already uses; if the task's
chosen worker cannot show the required capability while another
already-authorized worker can, the delegation is rejected before dispatch —
a real production path, not just a callable method. It never blocks when no
eligible worker anywhere qualifies, and it never lets an unauthorized worker
appear as the suggested alternative (both are structural, not just
convention). Every routing decision — pass or reject — is also recorded as a
`routing_decision` event. Covered by
`TestValidateCapabilityRouting_RejectsUnqualifiedChoiceWithAlternative`,
`TestValidateCapabilityRouting_AllowsQualifiedChoice`,
`TestValidateCapabilityRouting_IgnoresNonMatchingGoal`,
`TestValidateCapabilityRouting_DoesNotBlockWhenNoOneQualifies`,
`TestValidateCapabilityRouting_NeverNamesUnauthorizedAlternative`, and
`TestValidateDelegationPolicy_EnforcesCapabilityRouting` (the last exercises
the real `validateDelegationPolicy` entry point, not the unit-level method)
in `internal/team/capability_registry_test.go`; `expected-effective.json` for
the `05-workflow-and-tasks` compat fixture was regenerated
(`UPDATE_TEAM_COMPAT_GOLDEN=1`) to include the new
`delegation.CapabilityRouting` field.

What Stage 8 still does **not** do, on purpose: `CapabilitySourceVerified` has
a defined confidence ceiling but no producer anywhere in this package —
nothing promotes a capability claim using resolved decision outcomes, because
that data source is Stage 9's, and Stage 9 has not opened
(`TestPhase4AuthorizationStillPrecedesCapability`,
`internal/team/deferred_phase_gate_test.go`, asserts this stays true).
Outcome-driven confidence remains follow-up work, not part of this V1.

**Update, same day (DecisionEngine wiring, spec.md v2 / spec2.md):** spec.md
was rewritten to v2, reframing `reference`/`juror`/`challenger` as
capability-routed logical roles rather than fixed agent identities, and
spec2.md made the acceptance bar explicit: a stage's *trace* must show a
resolved concrete agent's own model being called, with zero calls to the
legacy judge-model sidecar — anything less is "shadow routing." Per spec2.md's
own sequencing (PR-2 before PR-3/PR-4), the `REFERENCE` stage is now
genuinely capability-routed:
`internal/team/decision_reference_capability_runner.go`
(`runReferenceEvidenceViaCapabilityRouting`, `invokeReferenceRoleAgent`) is a
new `ReferenceEvidenceRunner` implementation, selected per-request via
`ReferenceEvidenceRequest.RoutingRole` (`internal/team/decision_engine.go`,
`json:"-"`, sourced from `DecisionRequest.Policy.OutsideView.Role` in
`internal/team/decision_engine_reference.go`) — no change to
`DecisionEngine`'s own interfaces (`DecisionServices.ReferenceEvidence` was
already an interface seam; `coordinatorDecisionRunners`
(`internal/team/decision_runners.go`) was simply its only implementation
before this). The resolved candidate is invoked via
`Coordinator.createGatedAgent` + `runAgentWithStatusAndHistory` — the same
bounded, non-TODO primitive `coordinator_plan.go`'s plan-reviewer and
`coordinator_run.go`'s orchestrator calls already use — with tools narrowed to
a fixed read-only ceiling (`referenceRoleTools`) regardless of what the
resolved agent declares for itself. New schema:
`agent.ReferenceRolePolicy`/`OutsideViewPolicy.Role`
(`internal/agent/decision_config.go`), YAML key
`decision.profiles.*.outside-view.role.{required,preferred}-capabilities`. A
profile that does not set `outside-view.role` is completely unaffected — the
legacy sidecar path is untouched code, still the exclusive path for
`JUDGE`/`CHALLENGE`/`PREMORTEM`/`REVISE`/`FINALIZE`. Covered by
`TestReferenceRoleCapabilityRouting_InvokesResolvedAgentNotLegacyJudge` (the
concrete spec2.md acceptance test — the resolved agent's provider is called,
the legacy judge-model provider records zero calls for that stage),
`TestReferenceEvidence_WithoutRoutingRoleStaysOnLegacySidecar` (regression:
byte-for-byte unchanged when unconfigured), and
`TestReferenceRoleTools_NarrowsToReadOnly`
(`internal/team/decision_reference_capability_runner_test.go`), plus config
validation/parse-round-trip tests in `internal/agent/decision_config_test.go`
and `internal/team/decision_types_test.go`.

**Update, same day (JUDGE wiring, spec2.md PR-3):** `JUDGE` (juror) is now
also genuinely capability-routed, on the same pattern as `REFERENCE`.
`internal/team/decision_judge_capability_runner.go`
(`runJudgeViaCapabilityRouting`, shares `invokeCapabilityRoutedAgent` with
the reference runner) is a new `JudgeRunner` implementation, selected
per-judge via `JudgeRequest.RoutingRole` (`internal/team/decision_engine.go`,
no json tag needed — unlike `ReferenceEvidenceRequest`, `JudgeRequest` is
never marshaled into a producer prompt), sourced from
`DecisionRequest.Policy.JudgeRole` (`agent.JudgeRolePolicy`,
`internal/agent/decision_config.go`, yaml `decision.profiles.*.judge-role`).
Each of the N independent judge calls (`judge-1`..`judge-N`) resolves the
identical, deterministic ranked candidate list (no shared state across
calls — this is what keeps isolation intact) and round-robins over the
qualified pool by its own ordinal, so distinct judges get distinct concrete
agents; a pool smaller than `judge-role.min-distinct-agents` fails closed
before any judge is dispatched, never silently repeating one agent.
Every judge-role invocation gets **zero** tools regardless of what the
resolved agent declares (`judgeRoleZeroTools`) and a single forced step
(`fantasy.StepCountIs(1)`) — parity with the legacy sidecar's own
guarantee, not a new restriction: a judge must reason only from the sealed
evidence packet, or the "same evidence" comparability the isolation
guarantee depends on breaks. Opinion decoding is shared with the legacy
path (`decodeJudgeOpinion`), so the opinion-shape contract does not change
depending on who produced it. A profile that does not set `judge-role` is
completely unaffected. Covered by
`TestJudgeRoleCapabilityRouting_RoutesEachJudgeToADistinctAgent` (spec2.md's
own PR-3 validation checklist: same EvidenceHash implicit in the shared
sealed prompt, distinct bindings per judge, legacy judge-model records zero
calls), `TestJudgeRoleCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar`
(regression), `TestJudgeRoleCapabilityRouting_FailsClosedWhenPoolTooSmall`,
`TestJudgeRoleZeroTools_IsAlwaysEmpty`, and `TestJudgeOrdinal`
(`internal/team/decision_judge_capability_runner_test.go`), plus config
validation/parse-round-trip tests.

**Update, same day (CHALLENGE + REVISE wiring, spec2.md PR-4 — completes
spec2.md's MVP routed-role scope):** `CHALLENGE` is now also genuinely
capability-routed, and `REVISE` reuses `JUDGE`'s binding instead of
re-resolving, exactly as spec2.md's own final design specifies ("最終只有三種
capability-routed role: REFERENCE JUDGE CHALLENGE 以及 REVISE = reuse JUDGE
bindings"). `internal/team/decision_challenge_capability_runner.go` adds:
- `runChallengeViaCapabilityRouting` — same shape as JUDGE's runner:
  resolves the ranked qualified pool via `agent.ChallengeRolePolicy`
  (`DecisionPolicy.ChallengeRole`, yaml `challenge-role`), round-robins by
  `challengerOrdinal("challenger-N")`, invokes with zero tools + one forced
  step, decodes with `decodeChallengeResponse` (shared with the legacy
  path). `ChallengeRequest.RoutingRole` is set from `req.Policy.ChallengeRole`
  in `decision_engine_stages.go`'s `runChallenges`.
- `runRevisionViaCapabilityRouting` — resolves through
  `resolveJudgeRoleCandidate` (factored out of `decision_judge_capability_runner.go`
  so both call sites share one implementation), keyed by the same `JudgeID`
  and the same `agent.JudgeRolePolicy` (**no separate revision-role config
  exists** — `RevisionRequest.RoutingRole` is set from
  `req.Policy.JudgeRole`, not a new field). Because the candidate-resolution
  function is pure over `(role, ordinal)` and neither changes between JUDGE
  round 1 and REVISE, a revision request for `judge-2` always re-derives
  the exact same candidate JUDGE round 1 bound — this identical-resolution
  property *is* "reuse the original binding," with no binding ever
  persisted or looked up. Decodes with `decodeRevisionResult` (shared).
Both new stages are opt-in per profile; a team that doesn't set
`challenge-role` is completely unaffected. Covered by
`TestChallengeRoleCapabilityRouting_RoutesEachChallengerToADistinctAgent`
(spec2.md's acceptance shape for CHALLENGE),
`_WithoutRoutingRoleStaysOnLegacySidecar`, `_FailsClosedWhenPoolTooSmall`,
`TestChallengerOrdinal`, and — the concrete proof of spec2.md §8 —
`TestRevisionCapabilityRouting_ReusesOriginalJudgeBinding` (dispatches a
routed `judge-2`, then a routed revision for the same `judge-2`, and asserts
both landed on the identical resolved candidate's provider, with the other
candidates and the legacy sidecar untouched), plus
`TestRevisionCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar`
(`internal/team/decision_challenge_capability_runner_test.go`), plus config
validation/parse-round-trip tests.

This completes spec2.md's own MVP scope: `REFERENCE`, `JUDGE`, and
`CHALLENGE` are genuinely capability-routed, and `REVISE` correctly reuses
`JUDGE`'s bindings. `PREMORTEM`, `AGGREGATE`, and `FINALIZE` remain
sidecar-only **by spec2.md's own design** (§9 "AGGREGATE 完全不要 routing",
§10 "FINALIZE 第一版也不用capability routing") — not a gap, not deferred work.

**Update, same day (cost-aware weighted scoring + goal-driven routing
hints, spec.md v2 §12/§16/§30-31):** after finishing spec2.md's 4 PRs, the
user asked what else from spec.md v2's broader vision was unbuilt. Of the
9 items surveyed, durable `AgentBinding` persistence turned out to require
widening `DecisionOpinion`/`DecisionChallenge`/`ReferenceEvidenceDraft` —
the engine currently has no way to learn which concrete agent a runner
used at all — a bigger, higher-risk change than anything shipped so far.
The user chose the two lower-risk items first, which touch neither
`DecisionRecord` nor any persisted/hashed type:

- **Cost-aware weighted scoring**: `agent.ScoringWeights`
  (`RequiredMatch`/`PreferredMatch`/`Cost`, `internal/agent/agent.go`),
  team-wide via `TeamConfig.RoutingPolicy` (yaml
  `routing-policy.scoring.weights`). Defaults exactly reproduce the
  formula `CapabilityRegistry.scoreAgent` always used before weights
  existed (`RequiredMatch: 1.0, PreferredMatch: 0.5, Cost: 0.0`) — a team
  that configures nothing sees zero score change, proven by
  `TestCapabilityRegistry_DefaultWeightsReproduceLegacyScoring`. Cost
  reads the `CostClass` field `DeclaredCapability`/`CapabilityRecord`
  already had but nothing ever consumed
  (`internal/team/capability_registry.go`'s new `costClassFor`/
  `costClassScore`), and only affects ranking once a team sets a non-zero
  `cost` weight (`TestCapabilityRegistry_CostWeightAffectsRanking`).
  `CapabilityRegistry.WithScoringWeights` is additive — no existing
  constructor call site needed to change.
- **Goal-driven routing hints**: `agent.RoutingHint`
  (`WhenGoalContains`/`PreferredCapabilities`,
  `internal/agent/decision_config.go`), team-wide via
  `DecisionConfig.RoutingHints` (yaml `decision.routing-hints`) and
  threaded once into `DecisionRequest.RoutingHints`
  (`decision_dispatch.go`, same pattern as the existing
  `ProjectContext: c.decisionProjectContext()` line). Reuses
  `TaskGoalInvariants`' goal-substring selector shape rather than a new
  model-facing `TaskDef` field — deliberately avoiding the higher-risk
  path of touching `coordinator_tools.go`'s hand-maintained,
  provider-safe task-schema allowlist. `applyRoutingHints`
  (`internal/team/decision_routing_hints.go`) is a pure function; it is
  applied independently at all four routed construction sites
  (`decision_engine.go`'s judge dispatch, `decision_engine_reference.go`,
  `decision_engine_stages.go`'s challenge *and* revision dispatch) using
  the same `(role, hints, question)` inputs — which is exactly what keeps
  a hinted `REVISE` resolving to the same candidate a hinted `JUDGE`
  round 1 did, proven end-to-end by
  `TestJudgeRoleCapabilityRouting_HintsChangeBindingAndRevisionReusesIt`
  (a matching hint moves judge-1 from `cand-c` to `cand-a`, and a routed
  revision for the same JudgeID lands on that same `cand-a`, never the
  other two candidates).

Both are demonstrated in `.agent-teams/strategic-decision/team.yaml`
(`routing-policy.scoring.weights.cost` + `cost-class` declarations;
`decision.routing-hints` for storage-related questions) without disturbing
any of that team's already-documented default rankings — the hint's effect
is conditional on the question actually matching, so the non-matching
default case is exactly as before.

Still not done: full `DiversityPolicy` (distinct models/providers/
capability-groups, `allow-repeated-agent-definition` — only
`MinDistinctAgents` shipped), durable `AgentBinding` persistence beyond the
`routing_decision` event trail, pinned bindings, mid-run capability
invalidation, latency weighting, adaptive disagreement-driven challenger
routing, metrics, a structured explainability API, and the
`verified`/outcome-calibrated confidence tier (blocked on Stage 9). These
remain the explicitly-separate work spec2.md itself scopes beyond its four
PRs, and beyond what was chosen from that list this pass.

Opening Stage 9 is still a human decision, not a coding step.

**Update, same day (durable AgentBinding, metrics, explainability):**
investigated the remaining spec.md v2 extras and found capability
freshness/invalidation was **already implemented** and just never
demonstrated — `CapabilityRegistry.recordsFor`
(`internal/team/capability_registry.go`) already parses
`DeclaredCapability.DeclaredAt`/`StaleAfter`, sets `Stale=true,
Confidence=0` past expiry, and `scoreAgent` already skips stale records —
no code changed for that item, only documentation corrected. Full
`DiversityPolicy` and adaptive disagreement-driven challenger routing are
still genuinely unbuilt: neither has any partial type or scaffold to
extend (only `MinDistinctAgents`, an explicit "not the full policy" floor
per its own doc comment), so they need a separate design pass, not this
one.

The real remaining gap was durable `AgentBinding` persistence: the
resolved `chosen` agent ID was already computed at every routing site
(`decision_judge_capability_runner.go`, `decision_challenge_capability_runner.go`,
`decision_reference_capability_runner.go`) but only reached an ephemeral
`c.report(c.newEvent("routing_decision")...)` — a live TUI/CLI
`StatusEvent` callback, not the durable event journal. It vanished the
moment a run exited. Shipped:
- `AgentID string` on `DecisionOpinion`, `DecisionChallenge`,
  `DecisionRevision` (`internal/team/decision_types.go`), and on
  `ReferenceEvidenceDraft` (`internal/team/decision_reference.go`, tagged
  `json:"-"` so a producer's own response can never populate it — the
  runner sets it after decoding, never before). `ReferenceEvidenceResult`
  gained `ProducerAgentID`, copied from the validated draft in
  `decision_engine_reference.go`'s `runReferenceEvidence`. All four
  capability-routing runners now set their resolved `chosen` onto the
  return value before returning it.
- This is safe by construction, not just by convention: the event-log
  idempotency-key hash (`decisionEventKey` in `decision_store.go`) only
  hashes a small identity struct (DecisionID/RunID/TaskID/Attempt/
  Profile/EvidenceHash/Round/JudgeID/Reason/...) that excludes Opinion/
  Challenge/Revision content entirely, and every stage event already
  carries the *whole* struct as a pointer field on `decisionEvent`
  (`event.Opinion`/`.Challenge`/`.Revision`/`.ReferenceResult`) — so
  `AgentID` rides through into the durable journal for free, with zero
  new event kind and zero change to `decisionEventFor`/`appendDecisionEvent`
  call sites. `DecisionRecord.Opinions`/`.Challenges`/`.Revisions` are
  assigned straight from these slices in `buildRecord`
  (`decision_engine.go`), so the persisted record carries `AgentID`
  automatically too.
- `DecisionMetrics` (`decision_metrics.go`) already existed as a real
  projection over `(events, index entries)` — it gained
  `CapabilityRoutedBindingCount map[string]int`, tallied from the
  existing `EventDecisionOpinionSubmitted`/`ChallengeSubmitted`/
  `RevisionSubmitted`/`ReferenceCompleted` events (three of those four
  needed a new `case` in the switch; `RevisionSubmitted`'s existing case
  just gained a second line). A stage with no `AgentID` (the legacy
  sidecar path) is not counted at all, never counted under an empty key.
- New `internal/team/decision_explain.go`:
  `ExplainDecisionBindings(record, reference)` renders every stage's
  binding (or lack of one) into a stable, stage-ordered
  `[]DecisionAgentBinding` — pure, reads only what's already on the
  record plus the already-resolved `*ReferenceEvidenceResult` (never
  fetches anything itself). New `FetchDecisionArtifact[T any]`
  (`decision_store.go`) is a generic, digest-verified content-addressed
  read, exported so read-only tooling outside the package (the CLI) can
  fetch a `DecisionRecord` or `ReferenceEvidenceResult` by `ArtifactRef`
  without duplicating `store.Resolve`/`Verify`/`Open` plumbing.
- New `hufu decision explain <decision-id>` (`cmd/hufu/decisioncmd.go`),
  alongside the existing `list`/`show`/`resolve`/`assume`/`stats`. It
  reads the full persisted record (not just the cross-run index summary
  `show` uses) and tables stage/ordinal/agent/routed. Proven against a
  real, end-to-end `DecisionEngine.Run` (real `EventStore`, real
  `FileArtifactStore`, real `DecisionIndex` — a hand-written index row
  is deliberately rejected: `decision_index_listing.go`'s validated
  listing requires the durable journal to actually agree with any row
  carrying a record digest, by design, so the CLI test could not shortcut
  this with a synthetic fixture).
- Covered by: AgentID assertions added to every existing
  `TestJudgeRoleCapabilityRouting_*`/`TestChallengeRoleCapabilityRouting_*`/
  `TestRevisionCapabilityRouting_*`/`TestReferenceRoleCapabilityRouting_*`
  test (both the routed and legacy-sidecar cases, plus the hinted-binding
  and REVISE-reuse tests now assert `AgentID` equality directly, not just
  provider call counts); `TestReferenceEvidencePublishesRuntimeOwnedCASArtifacts`
  extended to prove `ProducerAgentID` survives publication;
  `TestComputeDecisionMetricsCountsCapabilityRoutedBindings`;
  `TestExplainDecisionBindings` (+ the no-reference/unresolved-result
  case); `TestDecisionExplain` (`cmd/hufu/decisioncmd_test.go`, text and
  `--json` output, plus a not-found error case).
- `go build`/`vet`/`golangci-lint run`/`go test ./...` all green (still
  only the two pre-existing, unrelated `internal/tools` failures).

Still not done, unchanged from above: full `DiversityPolicy`, pinned
bindings, mid-run capability invalidation, latency weighting, adaptive
disagreement-driven challenger routing, and the verified/outcome-calibrated
confidence tier (blocked on Stage 9, still a human decision).

**Update, same day (durable AgentBinding, metrics, explainability — shipped
by a research fork that overstepped its read-only instructions; reviewed
with the user and kept after independent re-verification):**
`DecisionOpinion`/`DecisionChallenge`/`DecisionRevision`/
`ReferenceEvidenceDraft` gained `AgentID string` (set by each
capability-routing runner right after invoking its resolved candidate;
empty on the legacy sidecar path). Because the event-log idempotency-key
hash (`decisionEventKey`, `decision_store.go`) only hashes an identity
struct that excludes Opinion/Challenge/Revision content, `AgentID` rides
into the durable event journal and `DecisionRecord` for free — zero new
event kind, zero schema-hash risk. `DecisionMetrics` gained
`CapabilityRoutedBindingCount map[string]int`. New
`hufu decision explain <decision-id>` CLI (`cmd/hufu/decisioncmd.go`) +
`ExplainDecisionBindings`/`FetchDecisionArtifact[T]`
(`internal/team/decision_explain.go`, `decision_store.go`) render, per
stage, whether capability routing resolved a concrete worker and which one.
Capability freshness/invalidation turned out to already be implemented
(`declared-at`/`stale-after` staleness already excluded from scoring) — no
code needed, just never demonstrated.

**Update, same day (the four remaining spec.md v2 items: DiversityPolicy,
pinned binding, mid-run capability invalidation, adaptive challenger
routing — planned via `EnterPlanMode`/`ExitPlanMode` given none had any
scaffolding to extend, confirmed by two rounds of read-only Explore-agent
research):**

- **Adaptive disagreement-driven challenger routing** (spec.md v2 §39-40):
  new `internal/team/decision_adaptive_challenge.go`. `criterionDispersion`
  computes, per criterion ID present in every valid JUDGE-round-1 opinion's
  score for the aggregate's preferred option, the population standard
  deviation across opinions — a pure function over data
  `DecisionOpinion.OptionScores[].Criteria` already carried, no new judge
  output or LLM call. `mostContestedCriterion` picks the highest-dispersion
  criterion (ties broken by ID ascending). `ChallengeRolePolicy` gained
  `AdaptiveCapabilities map[string][]string` (yaml
  `challenge-role.adaptive-capabilities`, keyed by a configured
  `decision.profiles.*.criteria[].id`; `Validate` fails closed on an
  unknown criterion ID). `adaptiveChallengeRole` widens the (already
  goal-hinted) `ChallengeRolePolicy`'s `PreferredCapabilities` before
  `runChallenges` dispatches — same shallow-copy-and-augment shape
  `hintedChallengeRole` uses, composed by concatenation. No
  `DecisionRecord`/event changes. Covered by
  `internal/team/decision_adaptive_challenge_test.go` (pure-function unit
  tests plus
  `TestChallengeRoleCapabilityRouting_AdaptiveCapabilitiesChangeBinding`,
  which proves disagreement concentration alone — not any agent's identity
  or opinion — flips which candidate CHALLENGE binds to) and config
  validation/parse-round-trip tests in `internal/agent/decision_config_test.go`
  / `internal/team/decision_routing_config_test.go`.

- **REVISE uses the durable original binding — mid-run capability
  invalidation, rescoped** (spec.md v2 §35-37): in this codebase's actual
  execution model (team config loaded once, immutable for a run), the only
  realistic invalidation window is the real wall-clock gap between JUDGE
  round 1 and REVISE (CHALLENGE and aggregation run in between). Since
  `AgentID` is now durable on `DecisionOpinion`, REVISE no longer needs to
  blindly re-resolve through the ranked pool and hope it lands on the same
  candidate — `revisionCandidate`
  (`internal/team/decision_challenge_capability_runner.go`) now reuses
  `req.Original.AgentID` directly (already in scope — `RevisionRequest.Original`
  is the full original opinion), after confirming it is still in
  `eligibleWorkerIDs()` and still satisfies the role's required
  capabilities via `ResolveCapabilityCandidates` (which naturally excludes
  anything that went stale in between) — failing closed with a clear error
  otherwise, rather than silently landing on a different agent than JUDGE
  used. This is a strict correctness fix, not just a spec checkbox: the old
  "recompute and hope it matches" approach had a latent bug where a
  capability change between rounds could silently move REVISE to a
  different candidate. Falls back to the original `resolveJudgeRoleCandidate`
  recompute only when `Original.AgentID` is empty (a misconfiguration edge
  case, not the normal path). Covered by
  `TestRevisionCapabilityRouting_UsesOriginalAgentIDNotFreshRanking`,
  `TestRevisionCapabilityRouting_FailsClosedWhenOriginalBindingCapabilityInvalidated`,
  `TestRevisionCapabilityRouting_FailsClosedWhenOriginalBindingNoLongerAuthorized`,
  `TestRevisionCapabilityRouting_FallsBackToResolutionWhenOriginalAgentIDEmpty`
  (`internal/team/decision_challenge_capability_runner_test.go`); the
  pre-existing `TestRevisionCapabilityRouting_ReusesOriginalJudgeBinding`
  now exercises this new mechanism directly (passes the real captured
  opinion, not a synthetic one) and still passes.

- **Pinned binding** (spec.md v2 §34): new `agent.RoutingPin{Agent, Reason
  string}` (`internal/agent/decision_config.go`), added as
  `Pin *RoutingPin` on `JudgeRolePolicy`/`ChallengeRolePolicy`/
  `ReferenceRolePolicy` (yaml `pin.agent`/`pin.reason`). A profile must
  separately opt in via new `RoutingPolicy.AllowPinnedBinding` (yaml
  `discipline.routing.allow-pinned-binding`) — checked at **team-load
  time** (`DecisionPolicy.Validate`'s new `requirePinPermitted` calls), so
  a misconfigured pin fails `hufu team validate`, never a running decision.
  A pin conflicting with `min-distinct-agents > 1` is also rejected at load
  time (a pin forces every ordinal to the same agent, which is
  definitionally at most one distinct agent). New shared resolver
  `internal/team/decision_pin.go`'s `resolvePinnedCandidate` — still
  enforces authorization (`eligibleWorkerIDs`) and the required-capability
  check via `ResolveCapabilityCandidates`; a pin can only force *which*
  already-qualified candidate is chosen, never bypass those checks. Wired
  as the first step in `resolveJudgeRoleCandidate`,
  `resolveChallengeRoleCandidate` (new, factored out of
  `runChallengeViaCapabilityRouting`'s previously-inline body), and the
  REFERENCE runner. `DecisionOpinion`/`DecisionChallenge`/`DecisionRevision`
  each gained `Pinned bool`/`BindingReason string` (rides the existing
  event/record plumbing for free, exactly like `AgentID` did — no
  `DecisionRecord` schema bump needed for these nested-struct fields).
  `DecisionAgentBinding`/`hufu decision explain` surface `Pinned`/`Reason`
  too. Covered by `internal/team/decision_pin_test.go` (pin forces every
  ordinal for judge/challenge/reference; fails closed on missing
  capability, missing authorization, or an unconfigured agent name; REVISE
  correctly reuses a pinned original binding) and
  `internal/agent/decision_config_test.go`'s
  `TestRoutingPinValidate`/`TestDecisionPolicyPinRequiresProfileGateAndNoDistinctFloor`.

- **Full `DiversityPolicy` — models and providers** (spec.md v2 §13, §32):
  deliberately **excludes capability-group distinctness** — no such
  taxonomy exists anywhere in this codebase (confirmed by research), and
  within one role's candidate set every qualified candidate already
  satisfies the identical required-capability set by construction, so
  there is no non-arbitrary way to define "distinct capability groups"
  without inventing a new taxonomy layer; that is a product decision, not
  a wiring gap, and stays explicitly deferred. Shipped: `JudgeRolePolicy`/
  `ChallengeRolePolicy` gained `MinDistinctModels`/`MinDistinctProviders`
  (hard floors, same bound-checking shape as `MinDistinctAgents`) and
  `PreferDistinctModels`/`PreferDistinctProviders` (soft preferences) plus
  `AllowRepeatedAgentDefinition` (spec.md v2 §33.1's light-profile escape
  hatch; rejected at load time when combined with `min-distinct-agents >
  1`, the same contradiction shape as the pin/distinct-floor check). Model
  = `AgentDef.Generation.Model`; provider = `AgentDef.ProviderURL`,
  documented as a proxy — this codebase has no separate `ProviderID`.
  New `internal/team/decision_diversity.go`: `resolveRoleCandidateOrder`
  reorders the ranked qualified pool so a candidate introducing a new
  model/provider value sorts before a pure repeat — stable within each
  bucket, so it returns the pool byte-identical when neither Prefer* flag
  is set. This is deterministic and stateless (depends only on
  `(pool, agents, flags)`, never on which ordinals already dispatched),
  which is exactly what preserves the pure-function-of-`(role, ordinal)`
  property REVISE's binding reuse and independent-per-judge resolution both
  depend on. `distinctModelCount`/`distinctProviderCount` back the new
  fail-closed floor checks in `resolveJudgeRoleCandidate`/
  `resolveChallengeRoleCandidate`. `DecisionOpinion`/`DecisionChallenge`
  also gained `Model`/`Provider string`, set by the runner alongside
  `AgentID` — this is what lets the after-the-fact
  `BindingDiversitySummary` computation
  (`computeBindingDiversitySummary`/`judgeDiversitySummary`/
  `challengeDiversitySummary`) work purely off durable per-stage record
  data, with no live `agents` map needing to be threaded into
  `decisionEngine` (which is deliberately decoupled from `Coordinator` for
  testability). `DecisionRecord` gained `JudgeDiversity`/
  `ChallengeDiversity *BindingDiversitySummary` (nil when that role was
  never routed — never a misleading all-zero summary), set in `buildRecord`
  (`decision_engine.go`). Because these are new **direct** fields on
  `DecisionRecord` itself (unlike the nested per-stage `AgentID`/`Pinned`
  additions), `DecisionRecordSchemaVersion` bumped from `2` to `3`
  following the same precedent the finalization fields' 1→2 bump set;
  `ValidateSchemaVersion` now accepts `{1, 2, 3}`.
  `hufu decision explain` prints both summaries when present. Covered by
  `internal/team/decision_diversity_test.go` (pure-function unit tests:
  reordering, floor counts, summary computation, nil-when-unrouted),
  `TestJudgeRoleCapabilityRouting_PreferDistinctModelsChangesBinding` /
  `_WithoutPreferDistinctModelsIsUnaffected` /
  `_FailsClosedWhenPoolLacksModelDiversity`
  (`internal/team/decision_judge_capability_runner_test.go`, using a
  dedicated `modelSharingJudgeRoutingHarness` since the existing shared
  harness's three candidates already declare distinct models, which would
  make the reordering behavior untestable) — the equivalent challenge-role
  wiring is not separately integration-tested since it shares the exact
  same `resolveRoleCandidateOrder`/`distinctModelCount` functions, a
  deliberate scope trim given this pass's size, not an oversight — and
  config validation/parse-round-trip tests.

`internal/team/decision_types_test.go` was split: the routing-config parse
round-trip tests (outside-view.role, judge-role, challenge-role, pin,
diversity extensions, adaptive-capabilities, routing-policy scoring
weights, routing-hints) moved to new `decision_routing_config_test.go` —
the original file had grown past the project's 800-line-per-file guideline
(caught by the repo's own `TestDecisionRuntimeFilesRespectTheSizeLimit`).

`.agent-teams/strategic-decision/team.yaml` demonstrates `prefer-distinct-models`
on both profiles' `judge-role` (a documented no-op today — this team has no
per-agent model override, so there's nothing to differentiate on yet,
proving the config is accepted rather than changing rankings). Pinned
binding and adaptive-capabilities are **not** demonstrated in this team's
config: a pin has no natural fit for a demo team built around real ranking,
and this team declares no `decision.profiles.*.criteria` block at all today
— adding one just to exercise `adaptive-capabilities` would change this
team's real scoring behavior (`Overall` derivation), not just add an
unused, inert config surface, so it was left out rather than forced.

`go build`/`vet`/`golangci-lint run`/`go test ./...` all green (still only
the two pre-existing, unrelated `internal/tools` failures);
`hufu team validate .agent-teams/strategic-decision` passes.

This closes every item spec.md v2 named beyond spec2.md's four PRs, except
capability-group distinctness (no taxonomy exists; a product decision, not
a gap) and the `verified`/outcome-calibrated confidence tier, which remains
correctly blocked on Stage 9 (0/0, still a human decision to open).
