# Context query offline evaluation

> Status: active
> Authority: reference
> Verified-Commit: `d237124e` baseline; final PR-01 source hashes in the companion report
> Supersedes: —
> Superseded-By: —

PR-01 of the [implementation plan](../implementation-plans/context-delivery.md) is validated for offline use. The [machine-readable report](context-query-offline-progress.json) binds final validation, four actual provider pairs and sixteen benchmark cases to 44 source hashes. Historical measurements are separately labelled. Production remains legacy; there is no production query-mode setting in this stage.

The builder preserves the v1 primary query/request identity. Hints come only from admitted constraints, literal Verify text, accepted dependency summaries and receipt-bound workset paths. Wrong-run, pending and unaccepted sources cannot supply hints. Supplemental exact/lexical search applies canonical scope/lifecycle and SQL allow-sets before LIMIT, counts all API-returned source rows, and has no vector capability.

Shared and private sources stage under one 100ms child deadline. Errors, timeouts and revision changes discard all staged supplements; outer cancellation remains an error. Required memory, legacy ranking/reinforcement, private tier precedence and delivery budgets remain intact. Private SQLite eligibility reads canonical rows and typed activation together, restricted to the requested worker. Supplemental FTS uses canonical primary-key lookup with equivalent nullable scope/validity predicates. Legacy SQL and the existing lineage overfetch remain unchanged. Active private lane merging uses the candidate-policy cap. Restricted ancestry retains the entire private legacy bundle, with no new private eligibility/search work.

Default manifests remain schema 2. Offline candidate manifests use schema 3 and bind actual per-source modes/fallbacks. Memory-injection query hashes use the matching composite identity. Existing evidence/invariant validators retain schema-specific fingerprint checks. Comparison runs create no exposure/credit; actual fake-provider integration runs use isolated test workspaces.

## Actual provider evidence

Two real coordinators run each authored fixture in separate workspaces, with the same K/token budget and scripted model-call count. The fake provider's received messages are checked against the exact durable manifest. Workset hints require a verified producer and accepted binding; dependency hints require an accepted result.

| Signal | Delivery recall, legacy → candidate | Irrelevant optional tokens | Delivery precision |
|---|---|---|---|
| constraints | 0 → 1 | 28 → 21 | 0 → 0.25 |
| Verify | 0 → 1 | 28 → 21 | 0 → 0.25 |
| accepted workset path | 0 → 1 | 31 → 24 | 0 → 0.20 |
| accepted dependency summary | 0 → 1 | 28 → 21 | 0 → 0.25 |

Required delivery recall is 1 and forbidden delivery 0 in both arms. Candidate retrieval recall is 1, with nine pre-injection candidates versus eight legacy candidates; retrieval precision is 1/9. Retrieval observations now distinguish candidates from the four items selected for injection, so a budget omission is not misreported as a retrieval miss. A private-owner case excludes foreign-worker memory from every received request. The no-hints positive control preserves delivery IDs, query counts and irrelevant tokens. Harmful/stale penalties and pure-comparison no-write behavior retain regression coverage.

## Measurements and limits

Each benchmark case has ten samples. Store setup/seed is excluded from standalone ranking/selector/compile measurements. Candidate ranking comparison includes hashing the complete canonical snapshot; it is not isolated production selector latency. The three-source selector uses a precomputed eligible universe; runtime private eligibility is verified separately. Full-harness cost includes fresh provider/SQLite setup and teardown and uses a no-hints fixture, measuring attribution overhead rather than active-query quality.

| Canonical rows | Legacy ranking p50 / p95 (ms) | Candidate comparison p50 / p95 (ms) | Active / fallback samples |
|---|---|---|---|
| 10 | 1.159 / 2.074 | 3.185 / 4.416 | 10 / 0 |
| 1,000 | 2.322 / 2.819 | 14.28 / 20.71 | 10 / 0 |
| 10,000 | 3.091 / 4.854 | 176.2 / 216.9 | 3 / 7 |

Strong-distractor three-source selection falls back on all ten samples at 1,000 and 10,000 rows, with p50/p95 of 100.8/103.0ms and 100.4/109.8ms. These are fallback samples, not successful improvement. The existing all-matching legacy MMR cost was not measured to completion or fixed; large-lineage/vector redesign remains excluded.

Compiler p50/p95 is 0.05337/0.2980ms for legacy and 0.06685/0.4375ms for candidate. Manifest construction is 0.03249/0.2571ms and 0.05438/0.1028ms. Full-harness means are 297.51ms and 308.14ms. Allocations, actual search/source-row counts and all sample summaries are in the JSON report. These synthetic measurements establish no production latency/cost acceptance or real-task success claim.

## Final validation

The final source passed full `go test ./...`, `go vet ./...`, `golangci-lint run` (0 issues), the affected context/team/evalharness race suite and all existing offline evals: context-routing 2/2, memory-learning 3/3 and retry-recovery 2/2. Four provider quality pairs and both ten-sample benchmark commands passed. Source hashes were checked after validation.

An earlier race run found a real concurrent append in the harness status-event slice. Writes now use a shared mutex and readers use a cloned snapshot; the extra-model baseline case remains enabled and passes race. Its underlying production admission/lifecycle blocker is still PR-02 work.

The original real-SQLite runtime test incorrectly required improvement within 100ms on every race/checkptr run. The contract allows complete timeout rollback and sets no production latency SLA. Active orchestration of all three sources now uses frozen actual canonical/activation fixture rows. A separate real-SQLite test requires either complete active selection or identical legacy bundles with all-source timeout attribution, rejecting partial publication. These contract cases passed ten repetitions; final race also covers the candidate-cap regression. The four real provider quality pairs still require improvement, with unchanged oracle labels and delivery budgets.

```bash
TMPDIR=/tmp GOTMPDIR=/tmp go test -race -p 1 ./internal/context ./internal/team ./internal/evalharness -run 'ContextQuery|Supplemental|Activation|PersistentRankingBehaviourIsLocked|ContextManifest|MemoryInjection|ContextDelivery|MMR' -count=1
TMPDIR=/tmp GOTMPDIR=/tmp go test ./internal/team -run '^$' -bench '^BenchmarkContextQuery' -benchtime=10x -count=1 -benchmem
TMPDIR=/tmp GOTMPDIR=/tmp go test ./internal/evalharness -run '^$' -bench '^BenchmarkContextQueryPairedHarness$' -benchtime=10x -count=1 -benchmem
```

PR-02 remains: fix the extra-model canonical admission/lifecycle relationship, provide explicit team-neutral opt-in, freeze policy/resume compatibility, expose historical query attribution and verify all runtime paths. Nothing in this offline stage activates existing teams or removes those requirements.
