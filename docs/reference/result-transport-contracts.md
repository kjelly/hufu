# Result transport contract boundaries

> Status: active
> Authority: reference
> Verified-Commit: —（本次工作樹；完整測試、race、vet、lint 已通過）
> Supersedes: —
> Superseded-By: —

Tool submission, runtime collection and external proposals preserve the same
common claims and completion requirements. Each adapter keeps its own trusted
inputs and transport provenance.

| Entry | Untrusted input | Canonical owner | Result source |
|---|---|---|---|
| Tool | `SubmitResultInput` from `submit_result` | `localResultAdmission` and the occurrence transaction | `submitted` |
| Runtime collector | Claims collected after work and its receipt are preserved | The same local admission, bound to the durable collection checkpoint | `collected` |
| External backend | `WorkerResultProposal` | `ExternalResultCanonicalizer`, independently verified workspace delta, then runtime storage | `external_provider_proposal` |

Successful decoding or admission does not mark a task done. Verification,
evidence delivery, workspace apply, terminal cleanup and final acceptance retain
their existing owners. `partial`, `failed` and `blocked` remain legal handoffs
without becoming successful completions. A `success` or `completed_with_gaps`
claim still needs the declared completion gates.

All three decoders accept one JSON value with unique object keys, including
nested objects; the result must then satisfy the adapter's object schema.
Trailing whitespace is allowed. Duplicate keys, another trailing JSON value
and trailing non-JSON text are rejected before claims are canonicalized.
External proposal identity, receipts, evidence and verification remain outside
the model-authored schema. Native file hashes and lengths come from observed
bytes; a provider cannot replace them with its own claims.

## Regression evidence

The cross-adapter tests use temporary workspaces, real local admission and
collection checkpoints, and the external canonicalizer with a real workspace
snapshot/delta. They compare common claims while separately checking each
transport's source and runtime-owned identity. Independent store IDs and receipt
data retain their original bytes.

| Contract | Tests |
|---|---|
| Legal statuses and common claims; admission leaves the task in progress | `TestResultTransportCommonClaimsAndCompletionBoundary` |
| Duplicate keys, trailing JSON/text and runtime identity overrides | `TestResultTransportDecodersRejectAmbiguousJSON` |
| Invalid status, empty summary, forged identity or receipt produces no accepted result | `TestResultTransportRejectsInvalidClaimsWithoutSuccess` |
| Artifact SHA-256 and length bind actual native bytes | `TestResultTransportArtifactHashesComeFromNativeBytes` |
| Source completion order does not change a selected downstream presentation | `TestDependencyResultViewCompletionOrderKeepsPresentation` |

These tests live in
[the transport contract suite](../../internal/team/result_transport_contract_test.go)
and [dependency replay coverage](../../internal/team/dependency_result_view_replay_test.go).

The broader boundary matrix reuses the existing suites:

| Boundary | Existing evidence |
|---|---|
| Verifier and acceptance can reject a reported success | `TestCodexSuccessStillNeedsCompletionGate`; `TestResultCollectionRecoveryPreservesDownstreamAndAcceptanceGates`; `TestCompletionGateRejectsFalseCompletionInputs` |
| Result-only repair and replay retain original work | `TestResultCollectionResumeCompletesDurableClaimsWithoutWorkerOrProvider`; `TestPreparedCandidateResumesWithoutGeneration`; `TestCodexResultRepairDoesNotReplayWorkspaceWrite` |
| Stale leases and concurrent/duplicate submissions | `TestOccurrenceTransitionRejectsStaleSubmissionBeforeProjection`; `TestDuplicateArtifactSubmissionLeavesFirstAuthoritative`; `TestCandidateConcurrentSubmissionAcceptsOnce`; `TestResultCollectionReservesOneConcurrentModelTurn` |
| CAS/event failures and interrupted publication | `TestArtifactTransactionRollbackClearsMembershipAndRetainsCASData`; `TestPreparedCandidateAppendFailureCannotCommit`; `TestResultCollectionPreparedCommitPersistsBeforePublication`; `TestCodexSuccessfulResultFailsClosedWhenTranscriptCASUnavailable` |
| Interrupted workspace apply is reconciled | `TestReconcileAttemptWorldsFinishesInterruptedApplies`; `TestReconcileAttemptWorldsBlocksAnApplyItCannotProve` |
| Plan-first, closed sequences, direct/nested and extra models retain their contracts | `TestRuntimeResultTransportPreservesPlanSubmissionSurface`; `TestRuntimeResultTransportRemovesReportingAuthority`; `TestIsolatedWorkerEntryPoints`; `TestFreshTodoOccurrenceRetainsExtraModelsFanout` |
| Trusted native action results retain their separate entry and evidence | `TestCoordinatorRuntimeActionCompletesTodoWithoutUsingCache`; `TestRuntimeActionBindsFrozenInputsAcrossReceiptResultAndEvidence` |

The external adapter retains its independent filesystem checks. An ordinary
proposal may drop an unverified proposed file; grounded tasks enforce their
stronger evidence requirements. Local-only submission fields and native action
outputs keep their existing contracts. Transport defaults and frozen historical
policy are unchanged. These fixtures establish deterministic runtime behavior;
they do not establish compatibility with a live provider or external agent.

The final working-tree verification passed on 2026-10-29: full tests, targeted
race, vet, lint (0 issues), documentation links and diff checks. The Go/tool
configuration source hashes remained unchanged across those gates. Existing
unrelated working-tree edits were retained during validation.
