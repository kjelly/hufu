# Issue 26 removed-case reconciliation

> Status: historical
> Authority: analysis
> Verified-Commit: `fbf8faaa55d0ea66bdda446a17ebf0a0eedc6b22` (measurement controller; immutable comparison sources below)
> Supersedes: —
> Superseded-By: —

Comparison: original `8f65a57c` → six-change endpoint `b3360c9a`, each with the
same separately identified #46 fixture-drain patch. This review is based on
the immutable source trees, their Git diff and the original normal artifact
`12984623919`, SHA-256
`8ce542170d165ab1fdb3c06a6cc02c24dc36ca3e73ca99fd41efc407ab3eb989`.
The ZIP digest was reverified on 2026-11-10. Its original `warm-1.jsonl`
inventories have 7,407 shared top-level identities, **29 removed and 183 added**.
These are full-suite terminal identities, not static file counts or subtest sums.

The first 15 removals below belong to #28/#29. The remaining 14 include
concurrent runtime/decision protocol changes, notably `134ad954`. A replacement
that deliberately changes a protocol is **not assertion equivalence**. This
distinction prevents a whole-revision time difference being presented as a
causal estimate of the six performance refactors.

## Performance-refactor removals (15)

| Removed top-level identity | Retained contract / precise change |
| --- | --- |
| `cmd/hufu: TestE2E_AltScreenEntry` | `TestE2E_TUILifecycle` requires alternate-screen entry after a real blocked-provider request and rendered prompt. |
| `cmd/hufu: TestE2E_BuildBinary` | Shared CLI build and real version/help/lifecycle execution still require a usable binary. The old `Size() >= 1000` heuristic was intentionally removed, not literally retained; successful real execution is the stronger product contract. |
| `cmd/hufu: TestE2E_NarrowTerminal_NoCrash` | `TestE2E_TUILifecycle` requires a fresh narrow warning after real resize and subsequent recovery; pure `TestResizeRenderingContract` covers view dimensions. |
| `cmd/hufu: TestE2E_NoHangOnExit` | `TestE2E_NonInteractiveVersion` requires bounded exit 0 and version output for null stdin and actual pipe cases. |
| `cmd/hufu: TestE2E_RapidResize` | Real resize/recovery and post-resize output in lifecycle; repeated dimensions in the pure rendering contract. |
| `cmd/hufu: TestE2E_Resize_NoDeadlock` | Lifecycle proves fresh narrow/recovered output and responsiveness to SIGINT, with one process waiter. |
| `cmd/hufu: TestE2E_Signal_SIGINT` | Lifecycle's first SIGINT requires a durable wrap-up checkpoint; second SIGINT requires exit 130 and alternate-screen restoration. |
| `cmd/hufu: TestE2E_StdinPipe` | Actual pipe branch of `TestE2E_NonInteractiveVersion`, with exit/output assertions. |
| `cmd/hufu: TestE2E_TerminalRecovery` | Lifecycle requires alternate-screen exit after entry and process completion; separate owner waits for reader cleanup. |
| `cmd/hufu: TestE2E_TranscriptSample` | Logging-only transcript inspection replaced by mandatory rendered-prompt readiness. No claim of byte-identical old assertions. |
| `internal/context: TestScopeMatrix_EmptyAgentIDSeesAgentChild_Query` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_Query`; original token-identical body retains private-result rejection and shared visibility. |
| `internal/context: TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchExact` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchExact`; token-identical private-result rejection. |
| `internal/context: TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchLexical` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchLexical`; token-identical private-result rejection. |
| `internal/context: TestScopeMatrix_EmptyAgentIDSeesAgentChild_Vector` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_Vector`; token-identical private-result rejection. |
| `internal/team: TestTodoToolHandleUpdatePreventErrorToDoneDup` | `TestTodoToolHandleUpdatePreventErrorToDone`; token-identical response-error and persistent error-status assertions. |

The original [duplicate map](duplicate-tests-2026-10-22.md) records Go scanner
token comparison, and the [PTY map and fault injections](pty-tests-2026-10-22.md)
record the preserved/strengthened transport, readiness, signal, restoration,
resize and isolation contracts. The pure rendering matrix was excluded from
the focused PTY timings but remains in the complete suite. The fixture patch
used in the new experiment adds drain/cleanup, without removing any assertion.

## Other runtime/protocol changes (14)

These names also disappear between the two whole revisions. Their actual
behavioral changes must stay visible rather than be counted as equivalent
duplicate elimination.

| Removed top-level identity | Endpoint evidence and limitation |
| --- | --- |
| `internal/sidecar: TestChooseAskUserResponseRetriesOnceWithTheProblem` | `TestMixedAskUserResponseRetainsGenerationRepair` keeps valid/corrected/unusable/provider-failure branches and rejected-reply quoting, but now applies to mixed questions. Closed questions use `TestClosedQuestionAndMixedQuestionUseSeparatePaths`. |
| `internal/sidecar: TestMatchSkillsJSON` | `TestMatchSkillsUsesOneTypedDecisionPerSkill` checks candidate evidence preservation, one decision per skill, relevance and abstention. Free-text/Markdown JSON parsing and confidence heuristics are superseded, not asserted equivalent. |
| `internal/sidecar: TestMatchTeamDeclinesLowConfidenceSelection` | `TestMatchTeamDeclinesUnsupportedSelection` keeps unresolved selection via typed `none`; old numeric confidence parsing is superseded. |
| `internal/sidecar: TestSelectAgentRequiresAuthorizedHighConfidenceSelection` | `TestSelectAgentRequiresAuthorizedTypedSelection` accepts authorized candidate, permits `none` and rejects invented choice. Old response/confidence format is superseded. |
| `internal/team: TestAdversarialVerifySkipsWithoutSidecar` | `TestAdversarialVerifyFailsClosedWithoutDecisionRuntime` deliberately reverses enabled-without-runtime behavior to fail closed; disabled verification remains a no-op. Not an equivalent assertion. |
| `internal/team: TestControlDecisionEventValidation` | `TestDecisionPrimitiveEventValidation` rejects receipt mismatch, invalid projection and requires recovery. Old legacy/shadow observation payload shape is retired, not equivalent field-by-field. |
| `internal/team: TestControlDecisionPointsApplyModes` | `TestRuntimeControlUsesPrimitiveAndSettledReplay`, `TestRuntimeControlProviderFailureDoesNotRunLegacy`, `TestRuntimeControlSystemOneDecisionAndNoNet` verify typed runtime selection, replay, budget, denial and no-MCP boundaries. Legacy mode routing changed. |
| `internal/team: TestControlDecisionRedactsRegisteredSecrets` | `internal/decisionrt/control: TestDecideRedactsContextAndSkipsOffPoints` verifies request redaction, caller immutability and disabled backend; old dual-path coordinator integration is no longer the same architecture. |
| `internal/team: TestControlDecisionShadowRunsBothSidesConcurrently` | Runtime control calls now use the typed primitive path rather than the old `runControlPlan` dual execution. The old shadow-concurrency assertion is superseded; it is not preserved by a renamed test. |
| `internal/team: TestDAGSchedulerBudgetExpiryWhileQueuedTaskWaitsForPermit` | `TestDAGSchedulerBudgetExhaustionWhileQueuedTaskWaitsForPermit` retains bounded drain, queued task failure, truthful detail and no queued provider call. It deterministically exhausts tokens after admission rather than depending on a 20 ms wall-clock race; the budget trigger differs. |
| `internal/team: TestDecisionPrimitiveAbstainFailureRetryAndLimits` | `TestDecisionPrimitiveAbstainSettledFailureAndLimits` preserves failure/abstention/call bound, but settled failure must no longer retry identical input. Explicitly changed assertion. |
| `internal/team: TestHufuCodeReviewReportRendersOptionalInvariantEvidence` | `TestHufuCodeReviewReportRendersValidInvariantStateEvidence` checks preserved/violated/unknown against omitted/empty/present/null evidence and rejects invalid/missing fields; adds schema-validity obligations. |
| `internal/team: TestHufuCodeReviewSynthesisReceivesPrepareOutputsWithoutCoordinatorConstraints` | `TestHufuCodeReviewSynthesisReceivesCompactPrepareStatusWithoutCoordinatorConstraints` verifies exact compact scope/test/handoff status, hides model-echo diagnostics, and keeps canonical outputs runtime-owned. Payload representation changes. |
| `internal/team: TestPlanReviewerAuxiliaryCompilationUsesReviewerBoundProfile` | `TestPlanReviewerPrimitivePromptIsNotRewrittenByCompiler` retains model binding and now requires the typed review prompt to bypass the context compiler. The compiler-use expectation intentionally changes. |

## Interpretation

All 29 removed identities are accounted for, but the whole revision pair is
**not a token-equivalent assertion workload**. The 183 additions and modified
shared tests remain part of the after workload. No six-refactor whole-project
speedup or full old-protocol assertion equivalence follows from these data.
The [completed hosted report](issue26-performance-acceptance-2026-11-10.md) reconciles
this map with all raw skip/outcome inventories, native execution and timing
medians. A map alone does not execute a contract or make a missing observation pass.
