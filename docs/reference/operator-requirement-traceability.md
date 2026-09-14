# Operator experience requirement traceability

> Status: active
> Authority: reference
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

This index maps every work item and adversarial scenario in the active
[operator-experience architecture](../architecture/operator-experience.md) to
repository evidence. A test name means an executable contract, not that every
platform or human-usability claim has passed. The release interpretation and
remaining manual gates are recorded in
[operator release readiness](operator-release-readiness.md).

## Work-item evidence

| Work item | Delivery state | Primary executable evidence |
|---|---|---|
| HF-UX-000 | delivered | `TestOperatorPhase0ContractInventory` |
| HF-UX-001A | delivered | `TestOperatorPhase0RootJSONGolden` |
| HF-UX-001B | delivered | `TestOperatorPhase0JourneyFixtures` |
| HF-UX-002 | harness delivered | `TestOperatorUsabilityMeasurementScript` |
| HF-UX-010A | delivered | `TestResolveWorkspacePathModes` |
| HF-UX-010B | delivered | `TestOperatorPhase0RootWorkspaceRemainsLegacyBase` |
| HF-UX-011A | delivered | `TestNormalizeSnapshotClonesPointersAndRejectsUnknownEnums` |
| HF-UX-011B | delivered | `TestBindReadTargetSelectionMatrix` |
| HF-UX-012A | delivered | `TestInspectOverviewDoesNotModifySuccessfulWorkspace` |
| HF-UX-013 | delivered | `TestInspectOverviewUsesGlobalOrdinalsAndLimitsLatestChanges` |
| HF-UX-020A | delivered | `TestActionJourneyMatrix` |
| HF-UX-020B | delivered | `TestSelectActionsPolicyDenyHasNoBypass` |
| HF-UX-021 | delivered | `TestShellRenderersQuoteMetacharactersWithoutChangingArgv` |
| HF-UX-022 | delivered | `TestExecutionSummaryFallbacks` |
| HF-UX-023 | delivered | `TestBindActiveMutationTargetRejectsStaleAttemptAndCompletedTask` |
| HF-UX-030 | delivered | `TestHelpAllShowsCanonicalGroupsAndSafetyFlags` |
| HF-UX-031A | delivered | `TestCanonicalRunMatchesLegacyExecutionEffects` |
| HF-UX-031B | delivered | `TestInspectOverviewPublishesExactResumeFacadeForInterruptedSession` |
| HF-UX-032 | delivered | `TestResolveOutputAliasAcceptsEquivalentAndRejectsConflict` |
| HF-UX-033 | delivered | `TestTeamCheckStaticIsReadOnlyAndMarksOnlineSkipped` |
| HF-UX-034 | delivered | `TestResolveAndCheckModelSeparatesWorkerAndCoordinatorTargets` |
| HF-UX-039 | delivered | `TestTUIProductionFilesStayBounded`; `TestIntegration_CompactMode` |
| HF-UX-040 | delivered | `TestOperatorSnapshotSummaryMatchesSharedRenderer` |
| HF-UX-041 | delivered | `TestThemeSwitchIsInstanceScopedAndPreservesInteractionState` |
| HF-UX-042 | delivered | `TestAutoThemeWatcherIsIdleUntilBackgroundChanges` |
| HF-UX-043 | delivered | `TestPassiveQuitDoesNotRequestOwnerWrapUp` |
| HF-UX-044 | delivered | `TestTerminalTextSanitizationAndCellWidth` |
| HF-UX-050 | delivered | `TestInspectContextSeparatesManifestsAndEnforcesPrivateScope` |
| HF-UX-051 | delivered | `TestInspectLearningSeparatesSignalsAndOmitsPrivateMemory` |
| HF-UX-052 | delivered | `TestOpenSQLiteReadOnlyQueriesPromotionsWithoutMutation` |
| HF-UX-053 | delivered | `TestPromotionReviewApproveAndApplyRemainSeparate` |
| HF-UX-054 | delivered | `TestSkillGraphRendersDraftName` |
| HF-UX-055 | delivered | `TestOperatorPanelUsesVerifiedDetailsAndSeparatedLearningSignals` |
| HF-UX-060 | delivered | `TestTeamCreateWizardPreviewsValidatesThenWrites` |
| HF-UX-061 | delivered | `TestDynamicCompletionIsBoundedBranchAndRunScoped` |
| HF-UX-062 | delivered | `TestGeneratedOperatorCommandReferenceIsCurrent` |
| HF-UX-063 | delivered | `TestCanonicalExamplesAreShellParseableAndReferenceExistingCommands` |
| HF-UX-070 | blocked-human | `TestOperatorUsabilityMeasurementScript` verifies the harness only; participant count remains zero |
| HF-UX-071 | partial engineering evidence | `TestOperatorOverviewPerformance` is opt-in and host-specific; native platform gaps remain |
| HF-UX-072 | delivered preview fallback | `TestExecutionSummaryFallbacks` |
| HF-UX-073 | engineering evidence delivered | `TestOperatorRequirementTraceabilityIsComplete` verifies this index; stable promotion remains blocked by HF-UX-070 |

## Adversarial scenario evidence

| Scenario | Executable evidence |
|---|---|
| UX-T01 | `TestOperatorPhase0HelpAndCompletionCreateNothing`; `TestDynamicCompletionMissingScopeOrStoreIsSilent` |
| UX-T02 | `TestTeamCheckStaticIsReadOnlyAndMarksOnlineSkipped` |
| UX-T03 | `TestOnlineTeamCheckDeadlineIsClassifiedWithoutInference` |
| UX-T04 | `TestCanonicalRunWorkspaceSemantics` |
| UX-T05 | `TestOperatorPhase0RootWorkspaceRemainsLegacyBase` |
| UX-T06 | `TestCanonicalRunRejectsFlagConflictsBeforeExecution` |
| UX-T07 | `TestValidateCanonicalRunSegments`; `TestCanonicalRunWorkspaceSemantics` |
| UX-T08 | `TestResolveWorkspacePathCanonicalizesExistingSymlink`; `TestResolveTeamWorkspacePathCanonicalizesWorkingDirectorySymlink` |
| UX-T09 | `TestBindReadTargetRejectsSessionAndPersistedScopeCollisions`; `TestBindActiveMutationTargetRejectsStaleAttemptAndCompletedTask` |
| UX-T10 | `TestBindReadTargetDoesNotPromoteUnverifiedProjectSelector`; `TestInspectOverviewReturnsCompleteStableSnapshotForFailedRun` |
| UX-T11 | `TestOperatorPhase0RootJSONGolden`; `TestCLIProcessExitContract` |
| UX-T12 | `TestResolveOutputAliasAcceptsEquivalentAndRejectsConflict`; `TestInspectOutputAliasConflictFailsBeforeReadingWorkspace` |
| UX-T13 | `TestSkillGraphMissingSnapshotPerFormat`; `TestSkillGraphExistingEmptySnapshotPerFormat` |
| UX-T14 | `TestInspectOverviewJSONFailureIsSingleDocumentAndSilentOnStderr`; `TestJSONOutputDoesNotReportAbortedRunAsCompleted` |
| UX-T15 | `TestEventFormatJSONLStderrContainsOnlyStatusEvents` |
| UX-T16 | `TestInspectCommandOverviewJSONUsesQuerySuccessContract`; `TestInspectOverviewReturnsCompleteStableSnapshotForFailedRun` |
| UX-T17 | `TestInspectOverviewNonterminalVerifyingIsNotInferredInterrupted` |
| UX-T18 | `TestInspectRunUsesCanonicalRunFinished`; `TestUpdate_FinishedMsgCanonicalStatus` |
| UX-T19 | `TestDeriveActivityPrecedenceAndUnknownStatus` |
| UX-T20 | `TestSelectActionsUnknownExternalEffectInspectsAndNeverRetries` |
| UX-T21 | `TestSelectActionsPolicyDenyHasNoBypass`; `TestPassiveQuitDoesNotRequestOwnerWrapUp` |
| UX-T22 | `TestBindActiveMutationTargetRejectsStaleAttemptAndCompletedTask` |
| UX-T23 | `TestInspectOverviewLearningFailureDegradesToDiagnosisWithoutRetry` |
| UX-T24 | `TestSnapshotIDIgnoresQueryTimeLiveStateAndActions`; `TestActionJourneyMatrix` |
| UX-T25 | `TestActionRevalidationIgnoresPresentationAndUnrelatedHead` |
| UX-T26 | `TestShellRenderersQuoteMetacharactersWithoutChangingArgv` |
| UX-T27 | `TestSafeDisplayTextStripsTerminalControlsRedactsAndBounds`; `TestTerminalTextSanitizationAndCellWidth` |
| UX-T28 | `TestInspectContextFailsClosedOnRedactionError`; `TestPromotionReviewApproveAndApplyRemainSeparate` |
| UX-T29 | `TestApplyCLIModelOverrides_ModelDoesNotFanOutToSidecarOrGuard`; `TestResolveAndCheckModelSeparatesWorkerAndCoordinatorTargets` |
| UX-T30 | `TestProjectTaskUsesAttemptAnchoredTargetsAndBothTranscriptRefs`; `TestInspectTaskUsesFrozenExecutionTargetAndHidesRawEvidence` |
| UX-T31 | `TestOpenSQLiteReadOnlyQueriesPromotionsWithoutMutation`; `TestOpenSQLiteReadOnlyDoesNotMigrateOrMutateDatabase` |
| UX-T32 | `TestPromotionReviewApproveAndApplyRemainSeparate`; `TestOperatorPanelUsesVerifiedDetailsAndSeparatedLearningSignals` |
| UX-T33 | `TestPromotionReviewRejectsStaleIntentAndDoubleApproveIsIdempotent` |
| UX-T34 | `TestPromotionReviewRejectsStaleIntentAndDoubleApproveIsIdempotent`; `TestCLIAcceptanceCase6_IdempotentReapplyAndStale` |
| UX-T35 | `TestPromotionReviewCommandRefusesNonTTYAndUnattended` |
| UX-T36 | `TestInspectLearningDistinguishesEmptyFromUnknown`; `TestContextPromotionNoEligibleAndDefaultOutputIsContentFree` |
| UX-T37 | `TestInspectLearningSeparatesSignalsAndOmitsPrivateMemory` |
| UX-T38 | `TestInspectContextSeparatesManifestsAndEnforcesPrivateScope`; `TestDynamicContextAndPromotionCompletionIsSharedScopeOnlyAndReadOnly` |
| UX-T39 | `TestOperatorSnapshotSummaryMatchesSharedRenderer` |
| UX-T40 | `TestThemeSwitchIsInstanceScopedAndPreservesInteractionState` |
| UX-T41 | `TestNoColorAndEpaperDisableColorAndMotion`; `TestValidateRunFlags` |
| UX-T42 | `TestAutoThemeWatcherIsIdleUntilBackgroundChanges`; `TestShouldRedrawTaskDisplay`; `TestTaskLogBufferIsBounded` |
| UX-T43 | `TestSmallTerminalLayoutsKeepStateWhatNextPath`; `TestEightyColumnLayoutUsesReadableCompactGroups` |
| UX-T44 | `TestBackgroundEventsDoNotStealDetailFocus` |
| UX-T45 | `TestOperatorSnapshotGenerationRejectsLateOldScope` |
| UX-T46 | `TestPassiveQuitDoesNotRequestOwnerWrapUp`; `TestDetailTerminalAttachStartsProcessCommand` |
| UX-T47 | `TestTeamCreateWizardPreviewsValidatesThenWrites`; `TestTeamCreateWizardCancelAndExistingTeamNeverOverwrite`; `TestTeamCreateWizardRefusesNonTTYBeforeWrite` |
| UX-T48 | `TestCanonicalRunFactoryFlagsAreInstanceScoped`; `TestTeamListFactoryDoesNotShareFlagValues` |
| UX-T49 | `TestBindReadTargetRejectsMalformedPersistedScope`; `TestInspectStorageErrorsDoNotCreateOrRepairStorage` |
| UX-T50 | `TestOperatorOverviewPerformance`; `TestTaskLogBufferIsBounded` |
| UX-T51 | `TestCanonicalRunMatchesLegacyExecutionEffects` |
| UX-T52 | `TestExecutionSummaryFallbacks`; `TestOperatorPhase0RootJSONGolden` |

## Manual and environment-bound evidence

- HF-UX-070 is not satisfied. The committed measurement corpus and script are
  ready, but no participant result may be invented or replaced with an LLM
  judgment.
- `TestOperatorOverviewPerformance` runs only when
  `HUFU_OPERATOR_PERF=1`; its numbers are evidence for the named host, not a
  universal latency assertion.
- Cross-builds prove compilation, not native terminal behavior. Darwin,
  Linux arm64, Zsh, PowerShell, PTY, and low-refresh claims retain the states
  listed in the release-readiness report.
