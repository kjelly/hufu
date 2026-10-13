# Context delivery evaluation

> Status: active
> Authority: reference
> Verified-Commit: PR-00 source digest in the companion report; code baseline `7312757e0187d471f16347656e887207b9db96ef`
> Supersedes: —
> Superseded-By: —

This records the offline diagnostic foundation for the [implementation plan](../implementation-plans/context-delivery.md). It supplements the [pre-change baseline](context-delivery-baseline.md). Query selection remains legacy. Synthetic fixtures establish contract behavior and known misses; they do not establish real task success rates.

The receiver decodes actual fake-provider messages. The transport observer supplies invocation identity without changing request bytes, tool authorization, accounting, or durable request fingerprints. Reports retain item IDs, counts, hashes and reasons; bodies stay in the disposable test process/workspace. An attempted transport observation alone is never delivery evidence.

`context-delivery` selectors use agent, durable task index, attempt, purpose and per-invocation step. Transport retries keep the same step. Every selected request checks its matching durable manifest. `forbidden-context` checks every received request, including fixture sentinels whose marker is absent. Assistant text, tool arguments, tool responses and tool schemas cannot satisfy injection assertions.

Optional delivery precision counts all delivered optional memory in the manifest, including records not seeded by a fixture. Relevance labels are authored eligible IDs, not ranking output. Zero denominators and unobserved data remain null with reasons. Tokens use the labeled rune estimator. Candidate rows count every row returned by a source before filtering, fusion and final limits; duplicates returned by different sources count separately. Vector API calls do not establish embedding counts; only a fake embedder that counts its own calls does.

Fixed evaluation time applies to canonical seed timestamps and eligibility through a context value; it does not replace the production clock. Timing uses the real monotonic clock. `WriteJSONReport` omits diagnostic timings without mutating evidence; `WriteMeasuredJSONReport` preserves routing, private retrieval, compilation, manifest-persistence and retrieval measurements. Shared retrieval occurs inside routing, so the total adds routing, private recall when observed, compilation and persistence without counting shared retrieval twice. Unobserved stages stay unavailable.

## Coverage evidence

Test names below are reproducible case identifiers. Existing tests remain the authority for contracts they already cover; a compiler or ranking test is not described as a provider-delivery test.

| Case identifier | Boundary and result |
|---|---|
| `context-routing/provider-visible-scope-lifecycle` | Actual worker request includes relevant shared memory and required goal; expired, future, rejected, superseded, foreign-team and foreign-worker records are absent from every request. Phase mismatch is checked for the worker invocation. |
| `TestContextQueryBaselineSignalMisses/{constraint-miss,verification-miss,bound-path-miss,accepted-dependency-miss}` | Canonical exact/lexical retrieval freezes four primary-query misses, with independent signal-query positive controls. This does not claim runtime authorization of the synthetic path/dependency hints. |
| `TestContextFixtureSeedPreservesExplicitZeroAndCanonicalSupersession` | Canonical seed API preserves zero priority/confidence, fixed time and supersession. |
| `TestContextFixtureRejectsInvalidVocabularyAndRanges` / `TestEvalFixtureStrictDecode` | Invalid vocabulary, non-finite confidence, invalid ranges and unknown YAML keys fail before execution. |
| `TestContextFixtureBranchAliasesRequireRealForkChronology` | Branch aliases require an explicit tree and checked durable events; before/after-fork and sibling labels cannot substitute for event chronology. |
| `TestWP3_AgentCannotSeeOtherAgentPrivate`, `TestWP3_PersistentModeRetrievesCrossSession`, `TestWP3_MaxTokensLimitEnforced` | Existing private recall authorization, tier and budget contracts. |
| `TestWP6RecallAuthorizesParentMemoryAtForkAcrossRetrievalSources`, `TestWP6SavedSessionMemoryRecallsAcrossForkAtWorkerScope` | Existing exact/lexical/vector final guards enforce fork cutoff and exclude siblings. No large-lineage recall improvement is claimed. |
| `TestContextDeliveryExistingLifecycleSuites` | Actual receiver/manifest assertions reuse all cases in core-lifecycle, retry-recovery, memory-learning, both fresh-session suites, plan-first-dependency, verbatim-dependency and legacy-execution-replay. Required goals remain visible; retry failure context is required on the second attempt. |
| `TestRunDirectAgentUsesWorkerSurfaceAfterInheritedCoordinatorPreflight`, `TestRequestAgentToolUsesTypedResultWorkerSurface` | Existing direct/nested integration tests now inspect every actual receiver request for required goal/instruction fragments. |
| `TestContextDeliveryAgentBackendReceivesCompiledRequiredFragments` | The existing fake app-server backend receives required compiled goal/result-protocol fragments at `turn/start`, without an OpenAI proxy. |
| `TestContextDeliveryParallelRetriesKeepStreamOrdinals`, `TestContextManifestKeepsConcurrentModelExecutionsDistinct` | Concurrent collector streams and existing manifest ownership; these are not successful extra-model runtime delivery evidence. |
| `TestCompileWorkerContextVerifyOmitsRawAndGenericHistory`, `TestAuxiliaryCompilerIsolatesHistoryAndPersistsPurpose` | Existing compiler isolation for VERIFY and auxiliary review. |
| `TestExecuteTaskFailsClosedWhenWorkerContextPreflightFails`, `TestContextCompiler_RequiredItemOverflow`, `TestCoordinatorCannotFitDoesNotEmitProviderRequest` | Typed required-context budget rejection and existing typed provider-admission rejection precede model calls. |
| `context-routing/cjk-goal-dispatch`, `TestContextDeliveryChineseSyntaxAndFakeEmbeddingContract` | Chinese lexical syntax remains safe with its known semantic limitation. A fixed fake embedding contract recalls the authored positive record and counts two seed embeddings plus one query embedding. |
| `TestCoordinatorContextAdmissionUsesTransientProjectionWithRealFantasyAgent`, `TestWorkerPrepareUsesCanonicalCompactionInputAndFailsClosedOnMandatoryCaps`, `TestContextWindowManagerPreservesPromptAndRuntimeSuffixAcrossCompaction` | Existing actual request shaping and compaction safeguards. |
| `TestInvocationPromptPreparerCarriesBoundContextAndHonorsProjection`, `TestAuxiliaryDiagnosticIdentityUsesExactManifestWithoutAuthority` | Sidecar compilation binds diagnostics to the exact invocation. Projection creates no durable usage; a child worker gets its own diagnostic identity. |
| `TestFallbackAcrossBackendsUsesTheCandidateBackend`, `TestProviderFailureWithoutFallback` | Existing fallback route and fail-closed boundaries; active-query attribution is still pending PR-02. |
| `TestCoordinatorRuntimeActionCompletesTodoWithoutUsingCache` | Existing deterministic runtime-action path, independently of model delivery. |

## Known runtime baseline blocker

PR-01 now has a [source-bound offline evaluation](context-query-offline-progress.md): four actual provider pairs repair the authored signal misses, with unchanged delivery budgets and required/forbidden gates. Retrieval metrics now observe candidates before injection rather than route-selected IDs; historical PR-00 metrics remain bound to their original source revision. Default production routing remains legacy. The extra-model blocker below is still a PR-02 prerequisite; its harness status-event collector is now synchronized so the characterization itself passes race.

`TestContextDeliveryExtraModelBaselineRejectsBeforeWorkerProvider` records a failing extra-model evaluation rather than labeling it successful delivery. Frozen startup policy rejects the changed isolated leaf definitions before any worker provider request. The same run also exposes a legacy/event-store dual-write mismatch. The two actual requests belong to the coordinator; worker delivery metrics are unavailable.

Before PR-02 can pass its extra-model gate, a coding agent must correct the leaf's relationship to the frozen parent policy, preserve indexed admitted targets across canonical attempt validation, and verify parent-owned lifecycle/receipt/context-manifest merging. It must then replace the negative characterization with successful concurrent provider assertions, and prove genuine policy/target drift still fails before a provider call. This is repository work, with no live models or operator architecture choice required. The existing lineage retrieval architecture remains outside scope.

## Reproduction

Use `TMPDIR=/tmp GOTMPDIR=/tmp` for Go tests and eval runs, as explained in the baseline. The plan lists the full test, vet, lint and eval gates. The targeted receiver suite is:

```bash
TMPDIR=/tmp GOTMPDIR=/tmp go test ./internal/evalharness ./internal/sidecar ./internal/team ./internal/context -run '^Test(ContextDelivery.*|ContextFixture.*|ContextQueryBaseline.*|AuxiliaryDiagnostic.*|CoordinatorDiagnostic.*|InvocationPromptPreparer.*|ExecuteTaskFailsClosedWhenWorkerContextPreflightFails)$'
```

The [machine-readable report](context-delivery-evaluation.json) records synthetic fixture hashes, source digest, query/learning policies and case results. No raw model messages or operator workload is retained. PR-01 still needs its paired quality report and measured benchmarks; PR-02 still needs production opt-in, snapshot compatibility and successful coverage of every applicable invocation path.

PR-00 validation passed: full `go test ./...`, `go vet ./...`, `golangci-lint run` (0 issues), the affected agent/context/evalharness/sidecar/team diagnostic paths under `go test -race`, all context-routing/memory-learning/retry-recovery eval cases, and `bin/check-docs`. The extra-model characterization test passes by accurately reporting the existing failed integration; it does not make that integration a passing delivery gate. A baseline-only probe against the archived `f3ca1ba6` production sources independently reproduced zero worker calls, two coordinator calls, startup policy drift and the dual-write mismatch.

Measure the fixed nine-row scope/lifecycle fixture with ten repetitions:

```bash
TMPDIR=/tmp GOTMPDIR=/tmp HUFU_CONTEXT_DELIVERY_MEASURED_REPORT=/tmp/context-delivery-measured.json go test ./internal/evalharness -run '^$' -bench '^BenchmarkContextDeliveryBaseline$' -benchtime=10x -count=1 -benchmem
```

The benchmark reports worker-stage p50/p95 and actual retrieval calls/source rows. Its allocations and wall time include fake-provider and SQLite harness setup/teardown. They are not isolated query-selector allocations. This baseline is distinct from PR-01's still-required 10/1,000/10,000-row supplemental-lane benchmarks.
