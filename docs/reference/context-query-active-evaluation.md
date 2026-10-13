# Context query active evaluation

> Status: active
> Authority: reference
> Verified-Commit: 0d6bfd74271d8c0c8720baa21b16d0af35a2fa30 baseline; final PR-02 source hashes in companion report
> Supersedes: —
> Superseded-By: —

PR-02 of the [implementation plan](../implementation-plans/context-delivery.md) is validated for explicit active opt-in. The [machine-readable report](context-query-active-evaluation.json) binds final validation, three-arm receiver pairs and seventeen ten-sample benchmarks to 83 source hashes. Its pre-commit status records evidence capture; the stage commit carries delivery. Default remains legacy, and no existing team was enabled.

## Implemented contract

Flat/versioned team manifests accept context-routing.query-mode legacy or active. Active snapshots pin version, limits, fusion weights and hash. Missing/legacy policy preserves existing hashes; same-mode resume works and changed mode/policy fails before provider access. Query mode independently controls optional retrieval, without enabling memory or credit.

Primary remains intact. Only admitted constraints, literal Verify, accepted dependency summaries and receipt-backed paths supply bounded exact/lexical hints. Scope, lifecycle, activation and SQL allow-sets gate inclusion. Three sources share one 100ms child deadline; errors, timeouts and revision changes discard every staged supplement. Restricted ancestry retains the whole private legacy bundle. No supplemental embeddings, models, tools or background work are added.

Active calls bind schema 3 effective source modes/fallbacks and composite memory query hashes; default remains schema 2. Fanout retains parent frozen definitions, indexed targets, private leaf transitions and pre-provider parent manifest persistence. Model/attempt memory identities distinguish leaves; receipts merge before parent completion and outcome credit shares the occurrence cap. SDK/admission normalization owns schema containers, preserving authorization digests. Failed route attempts remain visible in canonical compatibility projection.

Historical context explain-memory with retrieval-id and inspect context display query policy/hash and query_plan_required without reranking or query plaintext. Scores come from the manifest; unsaved counters are null/unavailable. Scoped reads remain required.

## Actual provider quality

Each arm uses a real coordinator, isolated workspace and actual fake-provider receiver. Production active matches the accepted offline candidate. Required recall is 1 and forbidden delivery is 0. Runtime-created workset IDs differ between runs; authored seed IDs, required fragments and actual token cost are compared.

| Signal | Legacy → production recall | Irrelevant optional tokens | API source rows |
|---|---|---|---|
| constraint-miss | 0 → 1 | 28 → 21 | 8 → 9 |
| verification-miss | 0 → 1 | 28 → 21 | 8 → 9 |
| bound-path-miss | 0 → 1 | 31 → 24 | 8 → 10 |
| accepted-dependency-miss | 0 → 1 | 28 → 21 | 8 → 9 |

Path source rows include an exact hit and duplicate API rows, counted before deduplication. Retrieval recall is 1 with unchanged injection K/token budgets, and vector calls do not increase. No-hints positive controls and private-owner/foreign-scope cases retain coverage.

## Runtime and failure evidence

Frozen-source tests cover normal DAG; actual direct/nested calls; overlapping extra-model requests and a pinned extra retry; public coordinator continuation; actual 429 primary and successful admitted fallback; production PrepareStep worker compaction with required fragments retained at the receiver; fake app-server input; same-mode resume and zero-access drift rejection; historical CLI/replay; schema 2/3 integrity and persistence failure before dispatch. Eight existing lifecycle suites also run with active configuration in copied teams. Full tests retain memory-off/fresh/verification isolation, CJK/fake-vector, branch restrictions, runtime actions, penalties and cancellation coverage.

The worker compaction fixture declares a 16,384-token window and 2,000-token history target to accommodate mandatory result schemas and protected input. It proves a transient admission compaction decision and received verified-history projection. Worker transient compaction intentionally does not increment the coordinator durable compaction counter.

## Cost measurements and limits

Seed/store setup is excluded from standalone measurements. Candidate comparison includes primary ranking and hashing the full canonical snapshot; it is not isolated production selector latency. Supplemental benchmarks use precomputed eligibility, with real runtime private eligibility checked separately. Full-harness cost includes fresh provider/SQLite setup, seed and teardown and uses a no-hints fixture.

| Rows | Legacy p50 / p95 (ms) | Candidate comparison p50 / p95 (ms) | Active / fallback samples |
|---|---|---|---|
| 10 | 1.77 / 1.897 | 3.517 / 5.524 | 10 / 0 |
| 1,000 | 1.482 / 1.961 | 16.96 / 20.15 | 10 / 0 |
| 10,000 | 3.914 / 4.719 | 197 / 246.1 | 1 / 9 |

Strong-distractor three-source selection at 1,000 and 10,000 rows falls back in all ten samples; shared-deadline fault cases also roll back completely. These prove bounded failure behavior, not successful quality improvement. Source rows and calls remain within fixed budgets. Existing all-matching legacy MMR/lineage cost remains outside this change.

| Full no-hints harness | Mean (ms) | Bytes/op | Allocs/op |
|---|---|---|---|
| legacy | 366.56 | 49,391,387 | 530,286 |
| candidate | 307.92 | 54,266,005 | 629,803 |
| production | 336.35 | 59,028,602 | 727,269 |

Compiler/manifest p50/p95, allocations and all query/fallback/source-row values are in the JSON report. Synthetic results establish neither real-task success nor production cost acceptance. True-model A/B, live refresh and large-lineage/vector redesign remain excluded.

## Final validation

Frozen source passed full go test -p 1 ./..., go vet ./..., golangci-lint run (0 issues), affected agent/context/team/evalharness/inspect race checks, receiver matrix and provider pairs. Offline evals passed: context-routing 2/2, memory-learning 3/3, retry-recovery 2/2. Both ten-sample benchmark commands passed. Documentation/source hashes are checked before commit; normal hooks and post-commit hash checks seal delivery.

Earlier failures are retained in report history: shared-schema races and leaf identity/admission defects were repaired, a fallback projection gap was fixed, and CLI fixture flag isolation was corrected. No oracle labels, quality budgets, deadlines or safety gates were relaxed.
